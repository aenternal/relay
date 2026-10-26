package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"relay/internal/domain"
)

type insomniaResource struct {
	ID       string `json:"_id"`
	Type     string `json:"_type"`
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
	raw      json.RawMessage
	path     string
}

type insomniaPair struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Disabled    bool   `json:"disabled"`
	Type        string `json:"type"`
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
}

// ParseInsomnia accepts only the legacy JSON v4 export. YAML, template plugins,
// scripts, and resources for other protocols are never executed or guessed.
func ParseInsomnia(data []byte) (Document, []Warning, error) {
	if err := GuardJSON(data); err != nil {
		return Document{}, nil, err
	}
	var input struct {
		Type      string            `json:"_type"`
		Format    int               `json:"__export_format"`
		Resources []json.RawMessage `json:"resources"`
	}
	if json.Unmarshal(data, &input) != nil || input.Type != "export" || input.Format != 4 || input.Resources == nil {
		return Document{}, nil, errors.New("only Insomnia JSON export format 4 is supported")
	}
	if len(input.Resources) > MaxRecords {
		return Document{}, nil, errors.New("Insomnia export exceeds the resource limit")
	}
	resources := make([]insomniaResource, 0, len(input.Resources))
	byID := map[string]insomniaResource{}
	var workspace insomniaResource
	for i, raw := range input.Resources {
		var resource insomniaResource
		if json.Unmarshal(raw, &resource) != nil || resource.Type == "" {
			return Document{}, nil, errors.New("invalid Insomnia resource")
		}
		if resource.ID == "" || len(resource.ID) > 256 || byID[resource.ID].ID != "" {
			return Document{}, nil, errors.New("Insomnia resource IDs must be unique and nonempty")
		}
		resource.raw, resource.path = raw, fmt.Sprintf("$.resources[%d]", i)
		resources, byID[resource.ID] = append(resources, resource), resource
		if resource.Type == "workspace" {
			if workspace.ID != "" {
				return Document{}, nil, errors.New("import one Insomnia workspace per file")
			}
			workspace = resource
		}
	}
	if workspace.ID == "" {
		return Document{}, nil, errors.New("Insomnia export has no workspace")
	}
	name := workspace.Name
	if name == "" {
		name = "Imported Insomnia workspace"
	}
	b := newExternal(name)
	if err := b.count(len(resources)); err != nil {
		return Document{}, nil, err
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(data, &root)
	b.unsupported(root, "$", "_type", "__export_format", "__export_date", "__export_source", "resources")
	collection := domain.Collection{ID: b.id("collection"), WorkspaceID: b.doc.Workspace.ID, Name: name}
	b.doc.Collections = append(b.doc.Collections, collection)
	// Reserve all external variable names before generating any credential
	// placeholders, so a source variable cannot shadow a generated secret.
	for _, resource := range resources {
		if resource.Type == "environment" {
			var input struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if json.Unmarshal(resource.raw, &input) != nil {
				return Document{}, nil, errors.New("invalid Insomnia environment")
			}
			for name := range input.Data {
				b.variableName[name] = true
			}
		}
	}
	folders := map[string]string{}
	visiting := map[string]bool{}
	var folder func(string, int) (string, error)
	folder = func(id string, depth int) (string, error) {
		if id == workspace.ID {
			return "", nil
		}
		if depth > 24 || visiting[id] {
			return "", errors.New("Insomnia folder hierarchy is cyclic or too deep")
		}
		if mapped, ok := folders[id]; ok {
			return mapped, nil
		}
		resource, ok := byID[id]
		if !ok || resource.Type != "request_group" {
			return "", errors.New("Insomnia request or folder parent is missing")
		}
		visiting[id] = true
		parent, err := folder(resource.ParentID, depth+1)
		if err != nil {
			return "", err
		}
		visiting[id] = false
		var object map[string]json.RawMessage
		_ = json.Unmarshal(resource.raw, &object)
		b.unsupported(object, resource.path, "_id", "_type", "parentId", "name", "created", "modified", "metaSortKey", "authentication")
		auth, err := b.insomniaAuth(object["authentication"], resource.path+".authentication")
		if err != nil {
			return "", err
		}
		value := domain.Folder{ID: b.id("folder"), WorkspaceID: b.doc.Workspace.ID, CollectionID: collection.ID, ParentID: parent, Name: resource.Name, Auth: auth}
		if value.Name == "" {
			value.Name = "Imported folder"
		}
		folders[id] = value.ID
		b.doc.Folders = append(b.doc.Folders, value)
		return value.ID, nil
	}
	for _, resource := range resources {
		switch resource.Type {
		case "workspace":
			var object map[string]json.RawMessage
			_ = json.Unmarshal(resource.raw, &object)
			b.unsupported(object, resource.path, "_id", "_type", "parentId", "name", "created", "modified", "authentication")
			auth, err := b.insomniaAuth(object["authentication"], resource.path+".authentication")
			if err != nil {
				return Document{}, nil, err
			}
			b.doc.Collections[0].Auth = auth
		case "request_group":
			if _, err := folder(resource.ID, 0); err != nil {
				return Document{}, nil, err
			}
		case "request":
			parent, err := folder(resource.ParentID, 0)
			if err != nil {
				return Document{}, nil, err
			}
			h, err := b.insomniaRequest(resource.raw, resource.path)
			if err != nil {
				return Document{}, nil, err
			}
			request := domain.RequestDefinition{ID: b.id("request"), WorkspaceID: b.doc.Workspace.ID, CollectionID: collection.ID, FolderID: parent, Name: resource.Name, Protocol: "http", HTTP: h}
			if request.Name == "" {
				request.Name = "Imported request"
			}
			b.doc.Requests = append(b.doc.Requests, request)
			if err := b.count(0); err != nil {
				return Document{}, nil, err
			}
		case "environment":
			env, err := b.insomniaEnvironment(resource, byID, workspace.ID)
			if err != nil {
				return Document{}, nil, err
			}
			b.doc.Environments = append(b.doc.Environments, env)
		default:
			b.warn("unsupported-resource", resource.path, "This resource type is unsupported and was omitted.")
		}
	}
	return b.finish()
}

func (b *externalBuilder) insomniaEnvironment(resource insomniaResource, resources map[string]insomniaResource, workspaceID string) (domain.Environment, error) {
	env := domain.Environment{ID: b.id("environment"), WorkspaceID: b.doc.Workspace.ID, Name: resource.Name, Variables: []domain.Variable{}}
	if env.Name == "" {
		env.Name = "Imported environment"
	}
	var chain []insomniaResource
	seen := map[string]bool{}
	current := resource
	for {
		if seen[current.ID] || len(chain) > 24 {
			return env, errors.New("Insomnia environment hierarchy is cyclic or too deep")
		}
		seen[current.ID] = true
		chain = append(chain, current)
		if current.ParentID == workspaceID {
			break
		}
		parent, ok := resources[current.ParentID]
		if !ok || parent.Type != "environment" {
			return env, errors.New("Insomnia environment parent is missing")
		}
		current = parent
	}
	type inheritedValue struct {
		value  json.RawMessage
		secret bool
	}
	merged := map[string]inheritedValue{}
	for i := len(chain) - 1; i >= 0; i-- {
		var input struct {
			Data      map[string]json.RawMessage `json:"data"`
			IsPrivate bool                       `json:"isPrivate"`
		}
		if json.Unmarshal(chain[i].raw, &input) != nil {
			return env, errors.New("invalid Insomnia environment data")
		}
		for name, value := range input.Data {
			merged[name] = inheritedValue{value: value, secret: input.IsPrivate}
		}
	}
	var object map[string]json.RawMessage
	_ = json.Unmarshal(resource.raw, &object)
	b.unsupported(object, resource.path, "_id", "_type", "parentId", "name", "created", "modified", "metaSortKey", "data", "dataPropertyOrder", "color", "isPrivate")
	names := map[string]bool{}
	for name := range merged {
		names[name] = true
	}
	for i, name := range externalSortedNames(names) {
		if err := b.count(1); err != nil {
			return env, err
		}
		value := merged[name]
		b.addVariable(&env, name, value.value, fmt.Sprintf("%s.data[%d]", resource.path, i), value.secret, true)
	}
	return env, nil
}

func (b *externalBuilder) insomniaRequest(raw json.RawMessage, path string) (*domain.HTTPDefinition, error) {
	var input struct {
		URL            string          `json:"url"`
		Method         string          `json:"method"`
		Headers        []insomniaPair  `json:"headers"`
		Parameters     []insomniaPair  `json:"parameters"`
		PathParameters []insomniaPair  `json:"pathParameters"`
		Authentication json.RawMessage `json:"authentication"`
		Body           json.RawMessage `json:"body"`
		Follow         string          `json:"settingFollowRedirects"`
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || json.Unmarshal(raw, &object) != nil {
		return nil, errors.New("invalid Insomnia HTTP request")
	}
	b.unsupported(object, path, "_id", "_type", "parentId", "name", "created", "modified", "metaSortKey", "url", "method", "headers", "parameters", "pathParameters", "authentication", "body", "settingFollowRedirects")
	if len(input.Headers) > 1024 || len(input.Parameters) > 1024 || len(input.PathParameters) > 1024 {
		return nil, errors.New("Insomnia request has too many fields")
	}
	h := externalHTTP()
	if input.Method != "" {
		h.Method = strings.ToUpper(input.Method)
	}
	var err error
	h.URL, h.Query, err = b.splitURL(input.URL, path+".url", true)
	if err != nil {
		return nil, err
	}
	// Insomnia normally stores URL query parameters separately. If an export
	// includes both, preserve both explicitly and report the ambiguity.
	if len(h.Query) > 0 && len(input.Parameters) > 0 {
		b.warn("query-sources-combined", path+".parameters", "Both URL query and structured parameters were retained; review duplicate keys.")
	}
	for i, pair := range input.Parameters {
		h.Query = append(h.Query, b.field(pair.Name, pair.Value, fmt.Sprintf("%s.parameters[%d]", path, i), !pair.Disabled, true, true))
	}
	if len(h.Query) > 1024 {
		return nil, errors.New("Insomnia request has too many query parameters")
	}
	for i, pair := range input.Headers {
		h.Headers = append(h.Headers, b.field(pair.Name, pair.Value, fmt.Sprintf("%s.headers[%d]", path, i), !pair.Disabled, true, true))
	}
	for i, pair := range input.PathParameters {
		h.PathParams = append(h.PathParams, b.field(pair.Name, pair.Value, fmt.Sprintf("%s.pathParameters[%d]", path, i), !pair.Disabled, true, false))
	}
	if auth, err := b.insomniaAuth(input.Authentication, path+".authentication"); err != nil {
		return nil, err
	} else if auth != nil {
		h.Auth = *auth
	}
	switch input.Follow {
	case "", "global", "on":
	case "off":
		h.FollowRedirects = false
	default:
		b.warn("unsupported-setting", path+".settingFollowRedirects", "The redirect setting was not recognized; Relay defaults are used.")
	}
	if err := b.insomniaBody(input.Body, h, path+".body"); err != nil {
		return nil, err
	}
	return h, nil
}

func (b *externalBuilder) insomniaAuth(raw json.RawMessage, path string) (*domain.HTTPAuth, error) {
	if !externalPresent(raw) {
		return nil, nil
	}
	var input struct {
		Type     string `json:"type"`
		Disabled bool   `json:"disabled"`
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
		Prefix   string `json:"prefix"`
		Key      string `json:"key"`
		Value    string `json:"value"`
		AddTo    string `json:"addTo"`
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || json.Unmarshal(raw, &object) != nil {
		return nil, errors.New("invalid Insomnia authentication")
	}
	b.unsupported(object, path, "type", "disabled", "username", "password", "token", "prefix", "key", "value", "addTo")
	auth := &domain.HTTPAuth{Type: "none"}
	if input.Disabled {
		b.omittedAuth(raw)
		return auth, nil
	}
	switch input.Type {
	case "", "inherit":
		b.omittedAuth(raw)
		auth.Type = "inherit"
	case "none", "noauth":
		b.omittedAuth(raw)
	case "basic":
		auth.Type = "basic"
		auth.Username = b.secret(input.Username, path+".username", true)
		auth.Password = b.secret(input.Password, path+".password", true)
	case "bearer":
		if input.Prefix != "" && !strings.EqualFold(input.Prefix, "Bearer") {
			b.omittedAuth(raw)
			b.warn("unsupported-auth-prefix", path, "A custom bearer prefix is unsupported; authentication was disabled.")
			break
		}
		auth.Type = "bearer"
		auth.Token = b.secret(input.Token, path+".token", true)
	case "apikey", "api-key":
		if input.AddTo != "header" && input.AddTo != "queryParams" && input.AddTo != "query" {
			b.omittedAuth(raw)
			b.warn("unsupported-auth-location", path, "The API key location is unsupported; authentication was disabled.")
			break
		}
		auth.Type, auth.KeyName = "api-key", b.text(input.Key, path+".key", true)
		auth.KeyValue = b.secret(input.Value, path+".value", true)
		auth.In = "header"
		if input.AddTo != "header" {
			auth.In = "query"
		}
	default:
		b.omittedAuth(raw)
		b.warn("unsupported-auth", path, "This authentication helper is unsupported; authentication was disabled.")
	}
	return auth, nil
}

func (b *externalBuilder) insomniaBody(raw json.RawMessage, h *domain.HTTPDefinition, path string) error {
	if !externalPresent(raw) {
		return nil
	}
	var input struct {
		MIMEType string         `json:"mimeType"`
		Text     string         `json:"text"`
		Params   []insomniaPair `json:"params"`
		FileName string         `json:"fileName"`
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || json.Unmarshal(raw, &object) != nil {
		return errors.New("invalid Insomnia body")
	}
	b.unsupported(object, path, "mimeType", "text", "params", "fileName")
	if input.FileName != "" {
		b.warn("file-body-omitted", path, "A file body was omitted; select the file again in Relay.")
		return nil
	}
	if len(input.Params) > 1024 {
		return errors.New("Insomnia body has too many fields")
	}
	mime, _, _ := strings.Cut(strings.ToLower(input.MIMEType), ";")
	switch strings.TrimSpace(mime) {
	case "application/x-www-form-urlencoded":
		h.BodyKind = "form"
		for i, pair := range input.Params {
			h.Form = append(h.Form, b.field(pair.Name, pair.Value, fmt.Sprintf("%s.params[%d]", path, i), !pair.Disabled, true, false))
		}
	case "multipart/form-data":
		h.BodyKind = "multipart"
		for i, pair := range input.Params {
			pairPath := fmt.Sprintf("%s.params[%d]", path, i)
			if pair.Type == "file" || pair.FileName != "" {
				b.warn("file-body-omitted", pairPath, "A multipart file was omitted; select the file again in Relay.")
				continue
			}
			if pair.Type != "" && pair.Type != "text" {
				b.warn("unsupported-body-field", pairPath, "An unsupported multipart field was omitted.")
				continue
			}
			field := b.field(pair.Name, pair.Value, pairPath, !pair.Disabled, true, false)
			h.Multipart = append(h.Multipart, domain.HTTPPart{Name: field.Name, Value: field.Value, Enabled: field.Enabled, ContentType: pair.ContentType})
		}
	case "application/graphql", "application/graphql+json":
		b.warn("unsupported-body", path, "GraphQL bodies are unsupported and were omitted.")
	default:
		if len(input.Params) > 0 {
			b.warn("unsupported-body", path, "Body parameters for this media type are unsupported and were omitted.")
		}
		if input.Text != "" {
			h.BodyKind = "raw"
			if strings.TrimSpace(mime) == "application/json" || strings.HasSuffix(strings.TrimSpace(mime), "+json") {
				h.BodyKind = "json"
			}
			h.Body = b.text(input.Text, path+".text", true)
			if input.MIMEType != "" {
				found := false
				for _, header := range h.Headers {
					found = found || strings.EqualFold(header.Name, "Content-Type")
				}
				if !found {
					h.Headers = append(h.Headers, domain.Header{Name: "Content-Type", Value: input.MIMEType, Enabled: true})
				}
			}
		}
	}
	return nil
}
