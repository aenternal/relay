package project

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"relay/internal/domain"
	"relay/internal/protobuf/importer"
)

// Export selects a single workspace. Neither the vault nor the filesystem is
// consulted; descriptor artifacts already belong to the loaded snapshot.
func Export(ctx context.Context, state domain.State, workspaceID string, opts Options) (Document, []Warning, error) {
	doc := Document{Format: Format, Version: Version}
	for _, item := range state.Workspaces {
		if item.ID == workspaceID {
			doc.Workspace = item
			break
		}
	}
	if doc.Workspace.ID == "" {
		return Document{}, nil, errors.New("selected workspace no longer exists")
	}
	for _, item := range state.Collections {
		if item.WorkspaceID == workspaceID {
			doc.Collections = append(doc.Collections, item)
		}
	}
	for _, item := range state.Folders {
		if item.WorkspaceID == workspaceID {
			doc.Folders = append(doc.Folders, item)
		}
	}
	for _, item := range state.Requests {
		if item.WorkspaceID == workspaceID {
			doc.Requests = append(doc.Requests, item)
		}
	}
	for _, item := range state.Environments {
		if item.WorkspaceID == workspaceID {
			doc.Environments = append(doc.Environments, item)
		}
	}
	for _, item := range state.ProtoSchemas {
		if item.WorkspaceID == workspaceID {
			doc.Schemas = append(doc.Schemas, Schema{Metadata: item, DescriptorSetBase64: item.DescriptorSetBase64})
		}
	}
	doc, warnings, err := Prepare(ctx, doc, opts)
	if err != nil {
		return Document{}, nil, err
	}
	// Transfer-local IDs also prevent opaque source identifiers from revealing
	// an accidentally reused secret reference or credential value.
	doc, err = Reidentify(doc)
	return doc, warnings, err
}

func ParseNative(ctx context.Context, data []byte) (Document, []Warning, error) {
	if err := GuardJSON(data); err != nil {
		return Document{}, nil, err
	}
	var doc Document
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&doc); err != nil {
		return Document{}, nil, errors.New("invalid native project document or unknown property")
	}
	if doc.Format != Format || doc.Version != Version {
		return Document{}, nil, errors.New("unsupported native project format or version")
	}
	return Prepare(ctx, doc, Options{IncludeBodies: true})
}

// Prepare makes a detached, privacy-filtered copy suitable for validation and
// transfer. Callers still validate graph ownership with storage before commit.
func Prepare(ctx context.Context, input Document, opts Options) (Document, []Warning, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, nil, err
	}
	if input.Format != Format || input.Version != Version {
		return Document{}, nil, errors.New("unsupported native project format or version")
	}
	if err := documentLimits(input); err != nil {
		return Document{}, nil, err
	}
	encoded, err := json.Marshal(input)
	if err != nil || len(encoded) > MaxBytes {
		return Document{}, nil, errors.New("project exceeds the 32 MiB transfer limit")
	}
	if err = GuardJSON(encoded); err != nil {
		return Document{}, nil, err
	}
	var doc Document
	if err = json.Unmarshal(encoded, &doc); err != nil {
		return Document{}, nil, errors.New("project could not be copied")
	}
	warnings, credentialPatterns, err := sanitizeDocument(&doc, opts)
	if err != nil {
		return Document{}, nil, err
	}
	for i := range doc.Schemas {
		if err := ctx.Err(); err != nil {
			return Document{}, nil, err
		}
		item := &doc.Schemas[i]
		if len(item.DescriptorSetBase64) == 0 || len(item.DescriptorSetBase64) > base64.StdEncoding.EncodedLen(importer.MaxTotalBytes) {
			return Document{}, nil, errors.New("project descriptor artifact exceeds its 16 MiB limit")
		}
		artifact, err := base64.StdEncoding.Strict().DecodeString(item.DescriptorSetBase64)
		if err != nil || len(artifact) == 0 || len(artifact) > importer.MaxTotalBytes {
			return Document{}, nil, errors.New("project descriptor artifact is not valid bounded base64")
		}
		for _, pattern := range credentialPatterns {
			if pattern != "" && bytes.Contains(artifact, []byte(pattern)) {
				return Document{}, nil, errors.New("a descriptor artifact contains a known credential; remove the credential from the schema before export or import")
			}
		}
		digest := sha256.Sum256(artifact)
		if item.Metadata.Checksum != hex.EncodeToString(digest[:]) {
			return Document{}, nil, errors.New("project descriptor checksum does not match")
		}
		reg, err := importer.DescriptorSet(ctx, artifact)
		if err != nil {
			if ctx.Err() != nil {
				return Document{}, nil, ctx.Err()
			}
			return Document{}, nil, errors.New("project descriptor artifact could not be restored offline")
		}
		actual := make([]string, 0, len(reg.FilesInfo()))
		for _, file := range reg.FilesInfo() {
			actual = append(actual, file.Name)
		}
		listed := slices.Clone(item.Metadata.Files)
		slices.Sort(actual)
		slices.Sort(listed)
		if !slices.Equal(actual, listed) {
			return Document{}, nil, errors.New("project descriptor file metadata does not match its artifact")
		}
		if len(item.Metadata.RootFiles) == 0 {
			return Document{}, nil, errors.New("project descriptor requires a root file")
		}
		for _, root := range item.Metadata.RootFiles {
			if !slices.Contains(actual, root) {
				return Document{}, nil, errors.New("project descriptor root file is missing")
			}
		}
		if len(item.Metadata.ImportedAt) > 64 {
			return Document{}, nil, errors.New("invalid project descriptor timestamp")
		}
		if _, err := time.Parse(time.RFC3339Nano, item.Metadata.ImportedAt); err != nil {
			return Document{}, nil, errors.New("invalid project descriptor timestamp")
		}
	}
	encoded, err = json.Marshal(doc)
	if err != nil || len(encoded) > MaxBytes {
		return Document{}, nil, errors.New("sanitized project exceeds the 32 MiB transfer limit")
	}
	return doc, warnings, nil
}

func documentLimits(doc Document) error {
	count := 1 + len(doc.Collections) + len(doc.Folders) + len(doc.Requests) + len(doc.Environments) + len(doc.Schemas)
	for _, item := range doc.Environments {
		count += len(item.Variables)
	}
	if count > MaxRecords || len(doc.Requests) > MaxRequests || len(doc.Schemas) > 64 {
		return errors.New("project exceeds its record, request, or schema limit")
	}
	return nil
}

func freshID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", errors.New("project ID generation failed")
	}
	data[6] = data[6]&15 | 64
	data[8] = data[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", data[:4], data[4:6], data[6:8], data[8:10], data[10:]), nil
}

// Reidentify prevents an imported project from overwriting any existing record.
// All references must resolve inside the one imported workspace.
func Reidentify(input Document) (Document, error) {
	encoded, err := json.Marshal(input)
	if err != nil || len(encoded) > MaxBytes {
		return Document{}, errors.New("invalid project size")
	}
	var doc Document
	if json.Unmarshal(encoded, &doc) != nil {
		return Document{}, errors.New("invalid project")
	}
	ids := map[string]string{}
	add := func(id string) error {
		if id == "" || len(id) > 256 || strings.TrimSpace(id) != id || strings.ContainsRune(id, '\x00') || ids[id] != "" {
			return errors.New("project has an invalid or duplicate record ID")
		}
		value, err := freshID()
		if err != nil {
			return err
		}
		ids[id] = value
		return nil
	}
	if err = add(doc.Workspace.ID); err != nil {
		return Document{}, err
	}
	for _, item := range doc.Collections {
		if err = add(item.ID); err != nil {
			return Document{}, err
		}
	}
	for _, item := range doc.Folders {
		if err = add(item.ID); err != nil {
			return Document{}, err
		}
	}
	for _, item := range doc.Requests {
		if err = add(item.ID); err != nil {
			return Document{}, err
		}
	}
	for _, item := range doc.Environments {
		if err = add(item.ID); err != nil {
			return Document{}, err
		}
		for _, variable := range item.Variables {
			if err = add(variable.ID); err != nil {
				return Document{}, err
			}
		}
	}
	for _, item := range doc.Schemas {
		if err = add(item.Metadata.ID); err != nil {
			return Document{}, err
		}
	}
	oldWorkspace := doc.Workspace.ID
	ref := func(old string, optional bool) (string, error) {
		if old == "" && optional {
			return "", nil
		}
		if ids[old] == "" {
			return "", errors.New("project contains a dangling record reference")
		}
		return ids[old], nil
	}
	owner := func(old string) error {
		if old != oldWorkspace {
			return errors.New("project contains a record from another workspace")
		}
		return nil
	}
	doc.Workspace.ID = ids[oldWorkspace]
	if doc.Workspace.ActiveEnvironmentID, err = ref(doc.Workspace.ActiveEnvironmentID, true); err != nil {
		return Document{}, err
	}
	for i := range doc.Collections {
		v := &doc.Collections[i]
		if err = owner(v.WorkspaceID); err != nil {
			return Document{}, err
		}
		v.ID, v.WorkspaceID = ids[v.ID], doc.Workspace.ID
	}
	for i := range doc.Folders {
		v := &doc.Folders[i]
		if err = owner(v.WorkspaceID); err != nil {
			return Document{}, err
		}
		v.ID, v.WorkspaceID = ids[v.ID], doc.Workspace.ID
		if v.CollectionID, err = ref(v.CollectionID, false); err != nil {
			return Document{}, err
		}
		if v.ParentID, err = ref(v.ParentID, true); err != nil {
			return Document{}, err
		}
	}
	for i := range doc.Requests {
		v := &doc.Requests[i]
		if err = owner(v.WorkspaceID); err != nil {
			return Document{}, err
		}
		v.ID, v.WorkspaceID = ids[v.ID], doc.Workspace.ID
		if v.CollectionID, err = ref(v.CollectionID, false); err != nil {
			return Document{}, err
		}
		if v.FolderID, err = ref(v.FolderID, true); err != nil {
			return Document{}, err
		}
		if v.GRPC != nil {
			if v.GRPC.SchemaID, err = ref(v.GRPC.SchemaID, true); err != nil {
				return Document{}, err
			}
		}
	}
	for i := range doc.Environments {
		v := &doc.Environments[i]
		if err = owner(v.WorkspaceID); err != nil {
			return Document{}, err
		}
		v.ID, v.WorkspaceID = ids[v.ID], doc.Workspace.ID
		for j := range v.Variables {
			v.Variables[j].ID = ids[v.Variables[j].ID]
		}
	}
	for i := range doc.Schemas {
		v := &doc.Schemas[i].Metadata
		if err = owner(v.WorkspaceID); err != nil {
			return Document{}, err
		}
		v.ID, v.WorkspaceID = ids[v.ID], doc.Workspace.ID
	}
	return doc, nil
}

func State(doc Document) domain.State {
	state := domain.State{Workspaces: []domain.Workspace{doc.Workspace}, Collections: doc.Collections, Folders: doc.Folders, Requests: doc.Requests, Environments: doc.Environments, Settings: domain.DefaultSettings(), UI: domain.UIState{OpenRequestIDs: []string{}}}
	for _, item := range doc.Schemas {
		schema := item.Metadata
		schema.DescriptorSetBase64 = item.DescriptorSetBase64
		state.ProtoSchemas = append(state.ProtoSchemas, schema)
	}
	return state
}
