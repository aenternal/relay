package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"relay/internal/domain"
)

type postmanPair struct {
	Key         string          `json:"key"`
	Value       string          `json:"value"`
	Type        string          `json:"type"`
	Disabled    bool            `json:"disabled"`
	ContentType string          `json:"contentType"`
	Source      json.RawMessage `json:"src"`
}

// ParsePostman imports the documented, non-executable HTTP subset of Collection
// v2.1. Other versions are rejected instead of being interpreted heuristically.
func ParsePostman(data []byte) (Document, []Warning, error) {
	if err := GuardJSON(data); err != nil {
		return Document{}, nil, err
	}
	var input struct {
		Info struct {
			Name   string `json:"name"`
			Schema string `json:"schema"`
		} `json:"info"`
		Items     []json.RawMessage `json:"item"`
		Auth      json.RawMessage   `json:"auth"`
		Variables []struct {
			Key      string          `json:"key"`
			Value    json.RawMessage `json:"value"`
			Type     string          `json:"type"`
			Disabled bool            `json:"disabled"`
		} `json:"variable"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return Document{}, nil, errors.New("invalid Postman Collection v2.1 structure")
	}
	if !postmanSchema(input.Info.Schema) || input.Items == nil {
		return Document{}, nil, errors.New("only Postman Collection v2.1 JSON is supported")
	}
	name := input.Info.Name
	if name == "" {
		name = "Imported Postman collection"
	}
	b := newExternal(name)
	var root map[string]json.RawMessage
	_ = json.Unmarshal(data, &root)
	b.unsupported(root, "$", "info", "item", "auth", "variable")
	env := domain.Environment{ID: b.id("environment"), WorkspaceID: b.doc.Workspace.ID, Name: "Collection variables", Variables: []domain.Variable{}}
	for i, variable := range input.Variables {
		if err := b.count(1); err != nil {
			return Document{}, nil, err
		}
		path := fmt.Sprintf("$.variable[%d]", i)
		if variable.Disabled {
			b.warn("disabled-variable-omitted", path, "A disabled collection variable was omitted.")
			continue
		}
		b.addVariable(&env, variable.Key, variable.Value, path, variable.Type == "secret", false)
	}
	if len(env.Variables) > 0 {
		b.doc.Environments = append(b.doc.Environments, env)
	}
	auth, err := b.postmanAuth(input.Auth, "$.auth")
	if err != nil {
		return Document{}, nil, err
	}
	collection := domain.Collection{ID: b.id("collection"), WorkspaceID: b.doc.Workspace.ID, Name: name, Auth: auth}
	b.doc.Collections = append(b.doc.Collections, collection)
	if err := b.postmanItems(input.Items, collection.ID, "", "$.item", 0); err != nil {
		return Document{}, nil, err
	}
	return b.finish()
}

func postmanSchema(schema string) bool {
	switch schema {
	case "https://schema.getpostman.com/json/collection/v2.1.0/collection.json", "http://schema.getpostman.com/json/collection/v2.1.0/collection.json", "https://schema.getpostman.com/json/draft-07/collection/v2.1.0/", "https://schema.postman.com/collection/json/v2.1.0/draft-07/collection.json":
		return true
	}
	return false
}

func (b *externalBuilder) postmanItems(items []json.RawMessage, collectionID, parentID, path string, depth int) error {
	if depth > 24 {
		return errors.New("Postman folder hierarchy is too deep")
	}
	for i, raw := range items {
		itemPath := fmt.Sprintf("%s[%d]", path, i)
		var item struct {
			Name    string            `json:"name"`
			Items   []json.RawMessage `json:"item"`
			Request json.RawMessage   `json:"request"`
			Auth    json.RawMessage   `json:"auth"`
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || json.Unmarshal(raw, &object) != nil || object == nil {
			return errors.New("invalid Postman collection item")
		}
		if err := b.count(1); err != nil {
			return err
		}
		b.unsupported(object, itemPath, "name", "id", "item", "request", "auth")
		if _, folder := object["item"]; folder {
			if externalPresent(item.Request) {
				return errors.New("Postman item cannot be both folder and request")
			}
			auth, err := b.postmanAuth(item.Auth, itemPath+".auth")
			if err != nil {
				return err
			}
			folder := domain.Folder{ID: b.id("folder"), WorkspaceID: b.doc.Workspace.ID, CollectionID: collectionID, ParentID: parentID, Name: item.Name, Auth: auth}
			if folder.Name == "" {
				folder.Name = "Imported folder"
			}
			b.doc.Folders = append(b.doc.Folders, folder)
			if err := b.postmanItems(item.Items, collectionID, folder.ID, itemPath+".item", depth+1); err != nil {
				return err
			}
			continue
		}
		if !externalPresent(item.Request) {
			return errors.New("Postman item has no HTTP request")
		}
		http, err := b.postmanRequest(item.Request, itemPath+".request")
		if err != nil {
			return err
		}
		request := domain.RequestDefinition{ID: b.id("request"), WorkspaceID: b.doc.Workspace.ID, CollectionID: collectionID, FolderID: parentID, Name: item.Name, Protocol: "http", HTTP: http}
		if request.Name == "" {
			request.Name = "Imported request"
		}
		b.doc.Requests = append(b.doc.Requests, request)
		if err := b.count(0); err != nil {
			return err
		}
	}
	return nil
}

func (b *externalBuilder) postmanRequest(raw json.RawMessage, path string) (*domain.HTTPDefinition, error) {
	h := externalHTTP()
	var shortURL string
	if json.Unmarshal(raw, &shortURL) == nil {
		var err error
		h.URL, h.Query, err = b.splitURL(shortURL, path, false)
		return h, err
	}
	var input struct {
		Method string          `json:"method"`
		URL    json.RawMessage `json:"url"`
		Header json.RawMessage `json:"header"`
		Body   json.RawMessage `json:"body"`
		Auth   json.RawMessage `json:"auth"`
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, errors.New("invalid Postman HTTP request")
	}
	b.unsupported(object, path, "method", "url", "header", "body", "auth")
	if input.Method != "" {
		h.Method = strings.ToUpper(input.Method)
	}
	if err := b.postmanURL(input.URL, h, path+".url"); err != nil {
		return nil, err
	}
	if externalPresent(input.Header) {
		var pairs []postmanPair
		if json.Unmarshal(input.Header, &pairs) != nil {
			var lines string
			if json.Unmarshal(input.Header, &lines) != nil {
				return nil, errors.New("invalid Postman headers")
			}
			for _, line := range strings.Split(strings.ReplaceAll(lines, "\r\n", "\n"), "\n") {
				if line == "" {
					continue
				}
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					return nil, errors.New("invalid Postman header line")
				}
				pairs = append(pairs, postmanPair{Key: strings.TrimSpace(key), Value: strings.TrimSpace(value)})
			}
		}
		if len(pairs) > 1024 {
			return nil, errors.New("Postman request has too many headers")
		}
		for i, pair := range pairs {
			h.Headers = append(h.Headers, b.field(pair.Key, pair.Value, fmt.Sprintf("%s.header[%d]", path, i), !pair.Disabled, false, true))
		}
	}
	if auth, err := b.postmanAuth(input.Auth, path+".auth"); err != nil {
		return nil, err
	} else if auth != nil {
		h.Auth = *auth
	}
	if err := b.postmanBody(input.Body, h, path+".body"); err != nil {
		return nil, err
	}
	return h, nil
}

func (b *externalBuilder) postmanURL(raw json.RawMessage, h *domain.HTTPDefinition, path string) error {
	var short string
	if json.Unmarshal(raw, &short) == nil {
		var err error
		h.URL, h.Query, err = b.splitURL(short, path, false)
		return err
	}
	var input struct {
		Raw      string          `json:"raw"`
		Protocol string          `json:"protocol"`
		Host     json.RawMessage `json:"host"`
		Port     string          `json:"port"`
		Path     json.RawMessage `json:"path"`
		Query    []postmanPair   `json:"query"`
		Variable []postmanPair   `json:"variable"`
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || json.Unmarshal(raw, &object) != nil || object == nil {
		return errors.New("invalid Postman request URL")
	}
	b.unsupported(object, path, "raw", "protocol", "host", "port", "path", "query", "variable")
	if input.Raw == "" {
		host, err := postmanSegments(input.Host, ".")
		if err != nil {
			return err
		}
		urlPath, err := postmanSegments(input.Path, "/")
		if err != nil {
			return err
		}
		input.Raw = host
		if input.Protocol != "" {
			input.Raw = input.Protocol + "://" + input.Raw
		}
		if input.Port != "" {
			input.Raw += ":" + input.Port
		}
		if urlPath != "" {
			input.Raw += "/" + strings.TrimPrefix(urlPath, "/")
		}
	}
	if _, explicitQuery := object["query"]; explicitQuery {
		if strings.Contains(input.Raw, "?") {
			b.warn("structured-query-used", path+".query", "Structured query parameters replace the raw URL query, preserving disabled rows and repeated keys.")
		}
		input.Raw, _, _ = strings.Cut(input.Raw, "?")
	}
	var err error
	h.URL, h.Query, err = b.splitURL(input.Raw, path, false)
	if err != nil {
		return err
	}
	if len(input.Query) > 1024 || len(input.Variable) > 1024 {
		return errors.New("Postman request has too many URL parameters")
	}
	for i, pair := range input.Query {
		h.Query = append(h.Query, b.field(pair.Key, pair.Value, fmt.Sprintf("%s.query[%d]", path, i), !pair.Disabled, false, true))
	}
	for i, pair := range input.Variable {
		h.PathParams = append(h.PathParams, b.field(pair.Key, pair.Value, fmt.Sprintf("%s.variable[%d]", path, i), !pair.Disabled, false, false))
	}
	return nil
}

func postmanSegments(raw json.RawMessage, separator string) (string, error) {
	if !externalPresent(raw) {
		return "", nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one, nil
	}
	var segments []string
	if json.Unmarshal(raw, &segments) != nil {
		return "", errors.New("unsupported Postman URL segments")
	}
	return strings.Join(segments, separator), nil
}

func (b *externalBuilder) postmanAuth(raw json.RawMessage, path string) (*domain.HTTPAuth, error) {
	if !externalPresent(raw) {
		return nil, nil
	}
	var input struct {
		Type string `json:"type"`
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, errors.New("invalid Postman authentication")
	}
	auth := &domain.HTTPAuth{Type: "none"}
	if input.Type == "noauth" {
		b.omittedAuth(raw)
		return auth, nil
	}
	if input.Type != "basic" && input.Type != "bearer" && input.Type != "apikey" {
		b.omittedAuth(raw)
		b.warn("unsupported-auth", path, "This authentication helper is unsupported; authentication was disabled.")
		return auth, nil
	}
	var pairs []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(object[input.Type], &pairs) != nil || len(pairs) > 32 {
		return nil, errors.New("invalid Postman authentication attributes")
	}
	values := map[string]string{}
	for i, pair := range pairs {
		var value string
		if json.Unmarshal(pair.Value, &value) != nil {
			return nil, errors.New("Postman authentication attributes must be text")
		}
		if _, exists := values[pair.Key]; exists {
			return nil, errors.New("duplicate Postman authentication attribute")
		}
		values[pair.Key] = value
		allowed := input.Type == "basic" && (pair.Key == "username" || pair.Key == "password") || input.Type == "bearer" && pair.Key == "token" || input.Type == "apikey" && (pair.Key == "key" || pair.Key == "value" || pair.Key == "in")
		if !allowed {
			b.warn("unsupported-auth-attribute", fmt.Sprintf("%s.attributes[%d]", path, i), "An unsupported authentication attribute was omitted.")
			if value != "" && externalWholeTemplate.FindStringSubmatch(value) == nil {
				b.rememberSecret(value, b.placeholder(true))
			}
		}
	}
	switch input.Type {
	case "basic":
		auth.Type = "basic"
		auth.Username = b.secret(values["username"], path+".username", false)
		auth.Password = b.secret(values["password"], path+".password", false)
	case "bearer":
		auth.Type = "bearer"
		auth.Token = b.secret(values["token"], path+".token", false)
	case "apikey":
		auth.Type = "api-key"
		auth.KeyName = b.text(values["key"], path+".key", false)
		auth.KeyValue = b.secret(values["value"], path+".value", false)
		auth.In = values["in"]
		if auth.In != "header" && auth.In != "query" {
			b.warn("unsupported-auth-location", path, "The API key location is unsupported; authentication was disabled.")
			auth = &domain.HTTPAuth{Type: "none"}
		}
	}
	return auth, nil
}

func (b *externalBuilder) postmanBody(raw json.RawMessage, h *domain.HTTPDefinition, path string) error {
	if !externalPresent(raw) {
		return nil
	}
	var input struct {
		Mode       string        `json:"mode"`
		Raw        string        `json:"raw"`
		Disabled   bool          `json:"disabled"`
		URLEncoded []postmanPair `json:"urlencoded"`
		FormData   []postmanPair `json:"formdata"`
		Options    struct {
			Raw struct {
				Language string `json:"language"`
			} `json:"raw"`
		} `json:"options"`
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || json.Unmarshal(raw, &object) != nil {
		return errors.New("invalid Postman request body")
	}
	b.unsupported(object, path, "mode", "raw", "disabled", "urlencoded", "formdata", "options")
	if input.Disabled {
		b.warn("disabled-body-omitted", path, "A disabled request body was omitted.")
		return nil
	}
	if len(input.URLEncoded) > 1024 || len(input.FormData) > 1024 {
		return errors.New("Postman request has too many body fields")
	}
	switch input.Mode {
	case "", "raw":
		h.BodyKind = "raw"
		if input.Options.Raw.Language == "json" {
			h.BodyKind = "json"
		}
		h.Body = b.text(input.Raw, path+".raw", false)
	case "urlencoded":
		h.BodyKind = "form"
		for i, pair := range input.URLEncoded {
			h.Form = append(h.Form, b.field(pair.Key, pair.Value, fmt.Sprintf("%s.urlencoded[%d]", path, i), !pair.Disabled, false, false))
		}
	case "formdata":
		h.BodyKind = "multipart"
		for i, pair := range input.FormData {
			pairPath := fmt.Sprintf("%s.formdata[%d]", path, i)
			if pair.Type == "file" || externalPresent(pair.Source) {
				b.warn("file-body-omitted", pairPath, "A multipart file was omitted; select the file again in Relay.")
				continue
			}
			if pair.Type != "" && pair.Type != "text" {
				b.warn("unsupported-body-field", pairPath, "An unsupported multipart field was omitted.")
				continue
			}
			field := b.field(pair.Key, pair.Value, pairPath, !pair.Disabled, false, false)
			h.Multipart = append(h.Multipart, domain.HTTPPart{Name: field.Name, Value: field.Value, Enabled: field.Enabled, ContentType: pair.ContentType})
		}
	default:
		b.warn("unsupported-body", path, "This body mode is unsupported; its body was omitted.")
	}
	return nil
}
