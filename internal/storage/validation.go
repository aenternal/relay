package storage

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"relay/internal/domain"
)

const (
	maxRecords       = 100_000
	maxSnapshotBytes = 32 << 20
	maxBodyBytes     = 8 << 20
)

var template = regexp.MustCompile(`^\{\{[A-Za-z_][A-Za-z0-9_.-]*\}\}$`)
var anyTemplate = regexp.MustCompile(`\{\{[A-Za-z_][A-Za-z0-9_.-]*\}\}`)

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

func sanitize(s domain.State) (domain.State, error) {
	count := len(s.Workspaces) + len(s.Collections) + len(s.Folders) + len(s.Requests) + len(s.Environments) + len(s.ProtoSchemas) + len(s.UI.OpenRequestIDs)
	if count > maxRecords {
		return domain.State{}, invalid("too many records")
	}
	budget := maxSnapshotBytes
	check := func(value string, limit int) bool {
		budget -= len(value)
		return budget >= 0 && len(value) <= limit && utf8.ValidString(value)
	}
	ids := map[string]bool{}
	checkID := func(id string) bool {
		if !check(id, 256) || strings.TrimSpace(id) != id || id == "" || strings.ContainsRune(id, '\x00') || ids[id] {
			return false
		}
		ids[id] = true
		return true
	}
	workspaces := make(map[string]domain.Workspace, len(s.Workspaces))
	collections := make(map[string]domain.Collection, len(s.Collections))
	folders := make(map[string]domain.Folder, len(s.Folders))
	requests := make(map[string]domain.RequestDefinition, len(s.Requests))
	envs := make(map[string]domain.Environment, len(s.Environments))
	for _, v := range s.Workspaces {
		if !checkID(v.ID) || !check(v.Name, 4096) {
			return domain.State{}, invalid("invalid workspace")
		}
		workspaces[v.ID] = v
	}
	schemas := make(map[string]domain.ProtoSchema, len(s.ProtoSchemas))
	if len(s.ProtoSchemas) > 64 {
		return domain.State{}, invalid("too many Protobuf schemas")
	}
	for _, v := range s.ProtoSchemas {
		if !checkID(v.ID) {
			return domain.State{}, invalid("invalid Protobuf schema ID")
		}
		if _, ok := workspaces[v.WorkspaceID]; !ok {
			return domain.State{}, invalid("Protobuf schema workspace missing")
		}
		if err := validateProtoSchema(v, check); err != nil {
			return domain.State{}, err
		}
		schemas[v.ID] = v
	}
	for _, v := range s.Collections {
		if !checkID(v.ID) || !check(v.Name, 4096) {
			return domain.State{}, invalid("invalid collection")
		}
		if _, ok := workspaces[v.WorkspaceID]; !ok {
			return domain.State{}, invalid("collection workspace missing")
		}
		if v.Auth != nil {
			if err := validateHTTPAuth(*v.Auth, check); err != nil {
				return domain.State{}, err
			}
		}
		collections[v.ID] = v
	}
	for _, v := range s.Folders {
		if !checkID(v.ID) || !check(v.Name, 4096) {
			return domain.State{}, invalid("invalid folder")
		}
		c, ok := collections[v.CollectionID]
		if !ok || c.WorkspaceID != v.WorkspaceID {
			return domain.State{}, invalid("folder collection ownership mismatch")
		}
		if v.Auth != nil {
			if err := validateHTTPAuth(*v.Auth, check); err != nil {
				return domain.State{}, err
			}
		}
		folders[v.ID] = v
	}
	// Iterative traversal avoids overflowing the Go stack for deeply nested folders.
	visited := make(map[string]bool, len(folders))
	for id := range folders {
		var chain []string
		current := id
		inChain := map[string]bool{}
		for current != "" && !visited[current] {
			if inChain[current] {
				return domain.State{}, invalid("folder cycle")
			}
			inChain[current] = true
			f := folders[current]
			chain = append(chain, current)
			if f.ParentID != "" {
				p, ok := folders[f.ParentID]
				if !ok || p.WorkspaceID != f.WorkspaceID || p.CollectionID != f.CollectionID {
					return domain.State{}, invalid("folder parent ownership mismatch")
				}
			}
			current = f.ParentID
		}
		for _, item := range chain {
			visited[item] = true
		}
	}
	for _, v := range s.Requests {
		if !checkID(v.ID) || !check(v.Name, 4096) {
			return domain.State{}, invalid("invalid request")
		}
		c, ok := collections[v.CollectionID]
		if !ok || c.WorkspaceID != v.WorkspaceID {
			return domain.State{}, invalid("request collection ownership mismatch")
		}
		if v.FolderID != "" {
			f, ok := folders[v.FolderID]
			if !ok || f.CollectionID != v.CollectionID || f.WorkspaceID != v.WorkspaceID {
				return domain.State{}, invalid("request folder ownership mismatch")
			}
		}
		variants := 0
		if v.HTTP != nil {
			variants++
		}
		if v.GRPC != nil {
			variants++
		}
		if v.WebSocket != nil {
			variants++
		}
		if variants != 1 {
			return domain.State{}, invalid("request must have one protocol definition")
		}
		switch v.Protocol {
		case "http":
			if v.HTTP == nil {
				return domain.State{}, invalid("request protocol mismatch")
			}
			h := v.HTTP
			if !check(h.Method, 256) || !check(h.Version, 256) || !check(h.URL, 65536) || !check(h.Body, maxBodyBytes) || len(h.Headers) > 1024 {
				return domain.State{}, invalid("HTTP definition too large")
			}
			if err := validateURL(h.URL); err != nil {
				return domain.State{}, err
			}
			if err := validateHTTPOptions(*h, check); err != nil {
				return domain.State{}, err
			}
			for _, header := range h.Headers {
				if !check(header.Name, 4096) || !check(header.Value, 65536) {
					return domain.State{}, invalid("HTTP header too large")
				}
				if sensitiveName(header.Name) && !safeHeaderTemplate(header.Name, header.Value) {
					return domain.State{}, invalid("sensitive header must reference an environment variable")
				}
			}
		case "grpc":
			if v.GRPC == nil {
				return domain.State{}, invalid("request protocol mismatch")
			}
			g := v.GRPC
			if !check(g.Endpoint, 65536) || !check(g.Method, 4096) || !check(g.Body, maxBodyBytes) {
				return domain.State{}, invalid("gRPC definition too large")
			}
			if !check(g.SchemaID, 256) || (g.BodyFormat != "" && g.BodyFormat != "json" && g.BodyFormat != "json5") {
				return domain.State{}, invalid("invalid gRPC schema or body format")
			}
			if g.SchemaID != "" {
				if schema, ok := schemas[g.SchemaID]; !ok || schema.WorkspaceID != v.WorkspaceID {
					return domain.State{}, invalid("gRPC schema workspace ownership mismatch")
				}
			}
			if err := validateGRPCOptions(*g, check); err != nil {
				return domain.State{}, err
			}
		case "websocket":
			if v.WebSocket == nil {
				return domain.State{}, invalid("request protocol mismatch")
			}
			if err := validateWebSocketOptions(*v.WebSocket, check); err != nil {
				return domain.State{}, err
			}
		default:
			return domain.State{}, invalid("unsupported request protocol")
		}
		requests[v.ID] = v
	}
	for _, v := range s.Environments {
		if !checkID(v.ID) || !check(v.Name, 4096) {
			return domain.State{}, invalid("invalid environment")
		}
		if _, ok := workspaces[v.WorkspaceID]; !ok {
			return domain.State{}, invalid("environment workspace missing")
		}
		count += len(v.Variables)
		if count > maxRecords {
			return domain.State{}, invalid("too many variables")
		}
		names := map[string]bool{}
		for _, variable := range v.Variables {
			if !checkID(variable.ID) || !check(variable.Name, 256) || strings.TrimSpace(variable.Name) != variable.Name || variable.Name == "" || names[variable.Name] || !check(variable.Value, 1<<20) || !check(variable.SecretRef, 256) {
				return domain.State{}, invalid("invalid or duplicate variable")
			}
			names[variable.Name] = true
			if variable.Secret && variable.Value != "" {
				return domain.State{}, invalid("secret value cannot be persisted")
			}
			if !variable.Secret && variable.SecretRef != "" {
				return domain.State{}, invalid("public variable cannot reference a secret")
			}
			if variable.SecretRef != "" && !validSecretReference(variable.SecretRef) {
				return domain.State{}, invalid("invalid secret reference")
			}
		}
		envs[v.ID] = v
	}
	for _, v := range s.Workspaces {
		if v.ActiveEnvironmentID != "" {
			e, ok := envs[v.ActiveEnvironmentID]
			if !ok || e.WorkspaceID != v.ID {
				return domain.State{}, invalid("active environment ownership mismatch")
			}
		}
	}
	if s.UI.WorkspaceID != "" {
		if _, ok := workspaces[s.UI.WorkspaceID]; !ok {
			return domain.State{}, invalid("active workspace missing")
		}
	}
	open := map[string]bool{}
	for _, id := range s.UI.OpenRequestIDs {
		r, ok := requests[id]
		if !ok || r.WorkspaceID != s.UI.WorkspaceID || open[id] {
			return domain.State{}, invalid("invalid open request")
		}
		open[id] = true
	}
	if s.UI.ActiveRequestID != "" && !open[s.UI.ActiveRequestID] {
		return domain.State{}, invalid("active request must be an open tab")
	}
	if s.Settings.Theme == "" {
		s.Settings.Theme = "system"
	}
	if s.Settings.ResponsePreviewBytes == 0 {
		s.Settings.ResponsePreviewBytes = domain.DefaultPreviewBytes
	}
	if s.Settings.HistoryLimit == 0 {
		s.Settings.HistoryLimit = domain.DefaultHistoryLimit
	}
	if s.Settings.Theme != "system" && s.Settings.Theme != "light" && s.Settings.Theme != "dark" {
		return domain.State{}, invalid("unsupported theme")
	}
	if s.Settings.ResponsePreviewBytes < 1024 || s.Settings.ResponsePreviewBytes > 128<<20 || s.Settings.HistoryLimit < 1 || s.Settings.HistoryLimit > 100_000 {
		return domain.State{}, invalid("settings limit out of range")
	}
	// Copy all mutable fields before redacting bodies. Frontend drafts are owned by
	// the caller and must retain their body when automatic snapshot saving occurs.
	s.Workspaces = append([]domain.Workspace{}, s.Workspaces...)
	s.Collections = append([]domain.Collection{}, s.Collections...)
	s.Folders = append([]domain.Folder{}, s.Folders...)
	for i := range s.Collections {
		if s.Collections[i].Auth != nil {
			value := *s.Collections[i].Auth
			s.Collections[i].Auth = &value
		}
	}
	for i := range s.Folders {
		if s.Folders[i].Auth != nil {
			value := *s.Folders[i].Auth
			s.Folders[i].Auth = &value
		}
	}
	s.Requests = append([]domain.RequestDefinition{}, s.Requests...)
	for i := range s.Requests {
		r := &s.Requests[i]
		if r.HTTP != nil {
			h := *r.HTTP
			h.Headers = append([]domain.Header{}, h.Headers...)
			h.Query = append([]domain.Header{}, h.Query...)
			h.PathParams = append([]domain.Header{}, h.PathParams...)
			h.Form = append([]domain.Header{}, h.Form...)
			h.Multipart = append([]domain.HTTPPart{}, h.Multipart...)
			// File grants are deliberately process-local capabilities. Persist names
			// and editor fields, never a stale grant or filesystem path.
			h.FileID = ""
			h.TLS.CAFileID = ""
			h.TLS.ClientCertFileID = ""
			h.TLS.ClientKeyFileID = ""
			for j := range h.Multipart {
				h.Multipart[j].FileID = ""
				if !h.PersistBody {
					h.Multipart[j].Value = ""
				}
			}
			if !h.PersistBody {
				h.Body = ""
				for j := range h.Form {
					h.Form[j].Value = ""
				}
			}
			r.HTTP = &h
		}
		if r.GRPC != nil {
			g := *r.GRPC
			g.Metadata = append([]domain.Header{}, g.Metadata...)
			g.TLS.CAFileID = ""
			g.TLS.ClientCertFileID = ""
			g.TLS.ClientKeyFileID = ""
			if !g.PersistBody {
				g.Body = ""
			}
			r.GRPC = &g
		}
		if r.WebSocket != nil {
			w := *r.WebSocket
			w.Headers = append([]domain.Header{}, w.Headers...)
			w.Query = append([]domain.Header{}, w.Query...)
			w.Subprotocols = append([]string{}, w.Subprotocols...)
			w.FileID, w.TLS.CAFileID, w.TLS.ClientCertFileID, w.TLS.ClientKeyFileID = "", "", "", ""
			if !w.PersistBody {
				w.Body = ""
			}
			r.WebSocket = &w
		}
	}
	s.Environments = append([]domain.Environment{}, s.Environments...)
	for i := range s.Environments {
		s.Environments[i].Variables = append([]domain.Variable{}, s.Environments[i].Variables...)
	}
	s.ProtoSchemas = append([]domain.ProtoSchema{}, s.ProtoSchemas...)
	for i := range s.ProtoSchemas {
		s.ProtoSchemas[i].Files = append([]string{}, s.ProtoSchemas[i].Files...)
		s.ProtoSchemas[i].RootFiles = append([]string{}, s.ProtoSchemas[i].RootFiles...)
	}
	s.UI.OpenRequestIDs = append([]string{}, s.UI.OpenRequestIDs...)
	return s, nil
}

func validSecretReference(ref string) bool {
	return ref != "" && len(ref) <= 256 && strings.TrimSpace(ref) == ref && !strings.ContainsRune(ref, '\x00') && utf8.ValidString(ref)
}

func normalizeSensitiveName(name string) string {
	return strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(name)))
}

func sensitiveName(name string) bool {
	n := normalizeSensitiveName(name)
	switch n {
	case "authorization", "proxyauthorization", "cookie", "setcookie", "apikey", "xapikey", "token", "accesstoken", "refreshtoken", "xauthtoken", "xtoken", "password", "passwd", "secret", "clientsecret", "signature", "credential", "credentials":
		return true
	}
	return strings.Contains(n, "apikey") || strings.HasSuffix(n, "token") || strings.HasSuffix(n, "secret") || strings.HasSuffix(n, "password") || strings.HasSuffix(n, "signature") || strings.HasSuffix(n, "credential")
}

func safeHeaderTemplate(name, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || template.MatchString(value) {
		return true
	}
	n := normalizeSensitiveName(name)
	if n == "authorization" || n == "proxyauthorization" {
		for _, scheme := range []string{"Bearer ", "Basic "} {
			if len(value) > len(scheme) && strings.EqualFold(value[:len(scheme)], scheme) && template.MatchString(value[len(scheme):]) {
				return true
			}
		}
	}
	return false
}

func validateURL(raw string) error {
	// Substitute references only for URL parsing, so variable hostnames are valid.
	u, err := url.Parse(anyTemplate.ReplaceAllString(raw, "variable"))
	if err != nil {
		return invalid("invalid request URL")
	}
	if u.User != nil {
		return invalid("URL credentials must use environment variables outside userinfo")
	}
	_, query, hasQuery := strings.Cut(raw, "?")
	if !hasQuery {
		return nil
	}
	query, _, _ = strings.Cut(query, "#")
	values, err := url.ParseQuery(query)
	if err != nil {
		return invalid("invalid request URL query")
	}
	for name, list := range values {
		if !sensitiveName(name) && normalizeSensitiveName(name) != "key" && normalizeSensitiveName(name) != "auth" && normalizeSensitiveName(name) != "code" {
			continue
		}
		for _, value := range list {
			if value != "" && !template.MatchString(value) {
				return invalid("sensitive URL query must reference an environment variable")
			}
		}
	}
	return nil
}
