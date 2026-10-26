package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"relay/internal/domain"
)

var externalVariable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)
var externalTemplate = regexp.MustCompile(`\{\{([^{}]*)\}\}`)
var externalWholeTemplate = regexp.MustCompile(`^\{\{([A-Za-z_][A-Za-z0-9_.-]*)\}\}$`)

// External exports are untrusted data. Reports contain paths and fixed messages,
// never source values, script contents, credentials, or remote filenames.
type externalBuilder struct {
	doc          Document
	warnings     []Warning
	omitted      int
	serial       int
	records      int
	generated    []domain.Variable
	secretNames  map[string]bool
	variableName map[string]bool
	removed      map[string]string
	secretBytes  int
	secretLimit  bool
	recordLimit  bool
}

func newExternal(name string) *externalBuilder {
	return &externalBuilder{doc: Document{
		Format: Format, Version: Version,
		Workspace:   domain.Workspace{ID: "external-workspace", Name: name},
		Collections: []domain.Collection{}, Folders: []domain.Folder{}, Requests: []domain.RequestDefinition{},
		Environments: []domain.Environment{}, Schemas: []Schema{},
	}, secretNames: map[string]bool{}, variableName: map[string]bool{}, removed: map[string]string{}}
}

func (b *externalBuilder) id(kind string) string {
	b.serial++
	return fmt.Sprintf("external-%s-%d", kind, b.serial)
}

func (b *externalBuilder) count(n int) error {
	b.records += n
	if b.records > MaxRecords || len(b.doc.Requests) > MaxRequests {
		return errors.New("external project exceeds the record or request limit")
	}
	return nil
}

func (b *externalBuilder) warn(code, path, message string) {
	if len(b.warnings) < MaxWarnings-1 {
		b.warnings = append(b.warnings, Warning{Code: code, Path: path, Message: message})
	} else {
		b.omitted++
	}
}

func (b *externalBuilder) unsupported(object map[string]json.RawMessage, path string, allowed ...string) {
	known := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		known[key] = true
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		if !known[key] && externalPresent(object[key]) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for i, key := range keys {
		switch key {
		case "event", "preRequestScript", "afterResponseScript", "scripts":
			b.warn("scripts-omitted", path+"."+key, "Scripts were omitted and are never executed during import or sending.")
			continue
		case "response", "responses":
			b.warn("responses-omitted", path+"."+key, "Saved responses and examples were omitted.")
			continue
		case "variable", "environment":
			b.warn("scoped-variables-omitted", path+"."+key, "Folder or request variable scopes are unsupported; scoped values were omitted.")
			continue
		case "flow", "flows":
			b.warn("flows-omitted", path+"."+key, "Flows and chained execution were omitted and are never executed.")
			continue
		case "description":
			b.warn("description-omitted", path+"."+key, "Descriptions are not part of the Relay request model and were omitted.")
			continue
		case "proxy", "certificate", "protocolProfileBehavior", "settingStoreCookies", "settingSendCookies", "settingDisableRenderRequestBody", "settingEncodeUrl", "settingRebuildPath":
			b.warn("settings-omitted", path+"."+key, "External runtime settings were omitted; Relay defaults are used.")
			continue
		}
		b.warn("unsupported-field", fmt.Sprintf("%s.unsupported[%d]", path, i), "An unsupported property was omitted.")
	}
}

func externalPresent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "[]" && s != "{}" && s != `""`
}

func (b *externalBuilder) placeholder(secret bool) string {
	if len(b.generated) >= MaxRecords {
		b.recordLimit = true
		return "{{import_limit_exceeded}}"
	}
	prefix := "import_value_"
	if secret {
		prefix = "import_secret_"
	}
	var name string
	for {
		name = prefix + strings.TrimPrefix(b.id("variable"), "external-variable-")
		if !b.variableName[name] {
			break
		}
	}
	b.variableName[name] = true
	b.generated = append(b.generated, domain.Variable{ID: b.id("variable"), Name: name, Secret: secret})
	return "{{" + name + "}}"
}

func (b *externalBuilder) text(value, path string, insomnia bool) string {
	// A single JSON string can contain arbitrarily many expressions despite the
	// JSON token budget. Bound generated variables before regex replacement.
	if len(externalTemplate.FindAllStringIndex(value, MaxRecords+1)) > MaxRecords {
		b.recordLimit = true
		return ""
	}
	unsupported := false
	if strings.Contains(value, "{%") || strings.Contains(value, "{#") {
		b.warn("unsupported-template", path, "Template tags and scripts are unsupported; replace this value before sending.")
		return b.placeholder(false)
	}
	value = externalTemplate.ReplaceAllStringFunc(value, func(match string) string {
		name := strings.TrimSpace(match[2 : len(match)-2])
		if insomnia {
			name = strings.TrimPrefix(name, "_.")
		}
		if !externalVariable.MatchString(name) {
			unsupported = true
			return b.placeholder(false)
		}
		return "{{" + name + "}}"
	})
	if unsupported || strings.Contains(value, "{{") && strings.Contains(externalTemplate.ReplaceAllString(value, ""), "{{") {
		b.warn("unsupported-template", path, "Only simple variable references are supported; dynamic expressions were omitted.")
	}
	return value
}

func (b *externalBuilder) secret(value, path string, insomnia bool) string {
	if value == "" {
		return ""
	}
	value = b.text(value, path, insomnia)
	if match := externalWholeTemplate.FindStringSubmatch(strings.TrimSpace(value)); match != nil {
		b.secretNames[match[1]] = true
		return strings.TrimSpace(value)
	}
	b.warn("secret-omitted", path, "A credential was replaced with an empty secret variable; configure it in the destination keyring.")
	replacement := b.placeholder(true)
	b.rememberSecret(value, replacement)
	return replacement
}

func (b *externalBuilder) rememberSecret(value, replacement string) {
	if value == "" || b.removed[value] != "" {
		return
	}
	b.secretBytes += len(value)
	if len(b.removed) >= 1024 || b.secretBytes > 1<<20 {
		b.secretLimit = true
		return
	}
	b.removed[value] = replacement
}

func (b *externalBuilder) omittedAuth(raw json.RawMessage) {
	// Unsupported authentication is omitted too, but its literal credentials may
	// also occur in an otherwise supported request body or environment value.
	var visit func(json.RawMessage, string)
	visit = func(raw json.RawMessage, name string) {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			switch name {
			case "type", "key", "in", "prefix", "addTo", "scope", "grantType", "grant_type":
				return
			}
			if value != "" && externalWholeTemplate.FindStringSubmatch(value) == nil {
				b.rememberSecret(value, b.placeholder(true))
			}
			return
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil && object != nil {
			for _, key := range externalSortedRawNames(object) {
				visit(object[key], key)
			}
			return
		}
		var array []json.RawMessage
		if json.Unmarshal(raw, &array) == nil {
			for _, item := range array {
				visit(item, name)
			}
		}
	}
	visit(raw, "")
}

func externalSortedRawNames(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func externalSensitive(name string) bool {
	n := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(name))
	switch n {
	case "authorization", "proxyauthorization", "cookie", "setcookie", "apikey", "xapikey", "token", "accesstoken", "refreshtoken", "xauthtoken", "xtoken", "password", "passwd", "secret", "clientsecret", "signature", "credential", "credentials", "key", "auth", "code":
		return true
	}
	return strings.Contains(n, "apikey") || strings.HasSuffix(n, "token") || strings.HasSuffix(n, "secret") || strings.HasSuffix(n, "password") || strings.HasSuffix(n, "signature") || strings.HasSuffix(n, "credential")
}

func (b *externalBuilder) field(name, value, path string, enabled, insomnia, sensitive bool) domain.Header {
	name = b.text(name, path+".name", insomnia)
	if sensitive && externalSensitive(name) {
		// Keep the common Authorization scheme, while excluding its credentials.
		prefix := ""
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
			for _, candidate := range []string{"Bearer ", "Basic "} {
				if len(value) >= len(candidate) && strings.EqualFold(value[:len(candidate)], candidate) {
					prefix, value = candidate, value[len(candidate):]
					break
				}
			}
		}
		value = prefix + b.secret(value, path+".value", insomnia)
	} else {
		value = b.text(value, path+".value", insomnia)
	}
	return domain.Header{Name: name, Value: value, Enabled: enabled}
}

func externalHTTP() *domain.HTTPDefinition {
	return &domain.HTTPDefinition{Method: "GET", Version: "auto", BodyKind: "none", Auth: domain.HTTPAuth{Type: "inherit"}, Proxy: domain.HTTPProxy{Mode: "none"}, FollowRedirects: true, MaxRedirects: 10, Decompress: true,
		Headers: []domain.Header{}, Query: []domain.Header{}, PathParams: []domain.Header{}, Form: []domain.Header{}, Multipart: []domain.HTTPPart{}}
}

func (b *externalBuilder) splitURL(raw, path string, insomnia bool) (string, []domain.Header, error) {
	raw = b.text(raw, path, insomnia)
	base, fragment, hadFragment := strings.Cut(raw, "#")
	if hadFragment && fragment != "" {
		b.warn("url-fragment-omitted", path, "The URL fragment was omitted because it is not sent in HTTP requests.")
	}
	base, query, hasQuery := strings.Cut(base, "?")
	// Preserve unresolved variable syntax while detecting userinfo. It is never
	// converted back into a stored URL or copied into diagnostics.
	parsed, err := url.Parse(externalTemplate.ReplaceAllString(base, "variable"))
	if err != nil {
		return "", nil, errors.New("invalid external request URL")
	}
	if parsed.User != nil {
		b.warn("url-credentials-omitted", path, "URL userinfo was omitted; configure authentication separately.")
		if scheme, rest, ok := strings.Cut(base, "://"); ok {
			userinfo, host, found := strings.Cut(rest, "@")
			if found {
				username, password, _ := strings.Cut(userinfo, ":")
				for _, credential := range []string{username, password} {
					if decoded, err := url.PathUnescape(credential); err == nil && decoded != "" && externalWholeTemplate.FindStringSubmatch(decoded) == nil {
						b.rememberSecret(decoded, b.placeholder(true))
					}
				}
				base = scheme + "://" + host
			}
		} else {
			return "", nil, errors.New("external URL userinfo is unsupported")
		}
	}
	if parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https" && !strings.HasPrefix(base, "{{") {
		return "", nil, errors.New("external importer supports HTTP requests only")
	}
	fields := []domain.Header{}
	if hasQuery && query != "" {
		for i, pair := range strings.Split(query, "&") {
			if i >= 1024 {
				return "", nil, errors.New("external request has too many query parameters")
			}
			key, value, _ := strings.Cut(pair, "=")
			key, err = url.QueryUnescape(key)
			if err != nil {
				return "", nil, errors.New("invalid external query encoding")
			}
			value, err = url.QueryUnescape(value)
			if err != nil {
				return "", nil, errors.New("invalid external query encoding")
			}
			fields = append(fields, b.field(key, value, fmt.Sprintf("%s.query[%d]", path, i), true, insomnia, true))
		}
	}
	return base, fields, nil
}

func (b *externalBuilder) addVariable(env *domain.Environment, name string, raw json.RawMessage, path string, secret, insomnia bool) {
	name = strings.TrimSpace(name)
	if !externalVariable.MatchString(name) {
		b.warn("unsupported-variable-name", path, "A variable whose name cannot be referenced was omitted.")
		return
	}
	for _, existing := range env.Variables {
		if existing.Name == name {
			b.warn("duplicate-variable", path, "A duplicate variable was omitted; the first definition is retained.")
			return
		}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		b.warn("unsupported-variable", path, "A variable with an invalid value was omitted.")
		return
	}
	var text string
	switch v := value.(type) {
	case nil:
	case string:
		text = b.text(v, path, insomnia)
	case bool, float64:
		// Preserve the source's number precision instead of reencoding float64.
		text = strings.TrimSpace(string(raw))
	default:
		b.warn("unsupported-variable-value", path, "Nested environment objects and arrays are unsupported; this variable is empty.")
	}
	secret = secret || externalSensitive(name) || b.secretNames[name]
	if secret {
		if text != "" {
			b.warn("secret-omitted", path, "A secret variable value was omitted; configure it in the destination keyring.")
			b.rememberSecret(text, "{{"+name+"}}")
		}
		text = ""
	}
	b.variableName[name] = true
	env.Variables = append(env.Variables, domain.Variable{ID: b.id("variable"), Name: name, Value: text, Secret: secret})
}

func (b *externalBuilder) finish() (Document, []Warning, error) {
	if b.recordLimit {
		return Document{}, nil, errors.New("external project exceeds the generated variable limit")
	}
	if len(b.doc.Environments) == 0 && (len(b.generated) > 0 || len(b.secretNames) > 0) {
		b.doc.Environments = append(b.doc.Environments, domain.Environment{ID: b.id("environment"), WorkspaceID: b.doc.Workspace.ID, Name: "Imported variables", Variables: []domain.Variable{}})
	}
	for i := range b.doc.Environments {
		env := &b.doc.Environments[i]
		if err := b.count(1); err != nil {
			return Document{}, nil, err
		}
		seen := map[string]bool{}
		for j := range env.Variables {
			variable := &env.Variables[j]
			seen[variable.Name] = true
			if b.secretNames[variable.Name] && !variable.Secret {
				b.rememberSecret(variable.Value, "{{"+variable.Name+"}}")
				variable.Value, variable.Secret = "", true
				b.warn("secret-omitted", fmt.Sprintf("environments[%d].variables[%d]", i, j), "A variable used by authentication was converted to an empty secret variable.")
			}
		}
		for _, v := range b.generated {
			if !seen[v.Name] {
				if err := b.count(1); err != nil {
					return Document{}, nil, err
				}
				v.ID = b.id("variable")
				if b.secretNames[v.Name] {
					v.Secret, v.Value = true, ""
				}
				env.Variables = append(env.Variables, v)
				seen[v.Name] = true
			}
		}
		for _, name := range externalSortedNames(b.secretNames) {
			if !seen[name] {
				if err := b.count(1); err != nil {
					return Document{}, nil, err
				}
				env.Variables = append(env.Variables, domain.Variable{ID: b.id("variable"), Name: name, Secret: true})
			}
		}
	}
	if len(b.doc.Environments) > 0 {
		b.doc.Workspace.ActiveEnvironmentID = b.doc.Environments[0].ID
	}
	if b.secretLimit {
		return Document{}, nil, errors.New("external project exceeds the credential redaction budget")
	}
	if err := b.scrubCredentials(); err != nil {
		return Document{}, nil, err
	}
	for i, request := range b.doc.Requests {
		if h := request.HTTP; h != nil && h.BodyKind == "json" && h.Body != "" && !json.Valid([]byte(h.Body)) {
			b.warn("json-body-review", fmt.Sprintf("requests[%d].http.body", i), "This JSON body requires review before sending. Relay resolves variable references only inside JSON strings; unquoted templates and invalid JSON are not evaluated.")
		}
	}
	if b.omitted > 0 {
		b.warnings = append(b.warnings, Warning{Code: "warnings-truncated", Path: "$", Message: fmt.Sprintf("%d additional import warnings were omitted from this bounded report.", b.omitted)})
	}
	return b.doc, b.warnings, nil
}

// Remove copies of credentials from ordinary fields as well (for example an
// Authorization token repeated in a saved example body). A trie-backed Replacer
// avoids rescanning the whole document separately for each source credential.
func (b *externalBuilder) scrubCredentials() error {
	keys := make([]string, 0, len(b.removed))
	for value := range b.removed {
		keys = append(keys, value)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) == len(keys[j]) {
			return keys[i] < keys[j]
		}
		return len(keys[i]) > len(keys[j])
	})
	var pairs []string
	seen := map[string]bool{}
	for _, value := range keys {
		for _, variant := range credentialVariants(value) {
			if !seen[variant] {
				pairs = append(pairs, variant, b.removed[value])
				seen[variant] = true
			}
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	replacer := strings.NewReplacer(pairs...)
	remaining := MaxBytes
	var scrubError error
	var walk func(reflect.Value, string)
	walk = func(value reflect.Value, field string) {
		if scrubError != nil {
			return
		}
		switch value.Kind() {
		case reflect.Pointer:
			if !value.IsNil() {
				walk(value.Elem(), field)
			}
		case reflect.Slice:
			for i := 0; i < value.Len(); i++ {
				walk(value.Index(i), field)
			}
		case reflect.Struct:
			for i := 0; i < value.NumField(); i++ {
				walk(value.Field(i), value.Type().Field(i).Name)
			}
		case reflect.String:
			if strings.HasSuffix(field, "ID") || field == "Format" || field == "Protocol" || field == "Type" || field == "BodyKind" || field == "Version" || field == "Mode" || field == "In" || field == "SecretRef" || !value.CanSet() {
				return
			}
			source := value.String()
			matches := externalTemplate.FindAllStringIndex(source, -1)
			out := externalLimitedText{remaining: &remaining}
			last := 0
			for _, match := range matches {
				if _, err := replacer.WriteString(&out, source[last:match[0]]); err != nil {
					scrubError = err
					return
				}
				if _, err := out.Write([]byte(source[match[0]:match[1]])); err != nil {
					scrubError = err
					return
				}
				last = match[1]
			}
			if _, err := replacer.WriteString(&out, source[last:]); err != nil {
				scrubError = err
				return
			}
			value.SetString(out.text.String())
		}
	}
	// Preserve variable identifiers while scrubbing their values. Other Name
	// fields are display labels or outbound header names, so copies are removed.
	for i := range b.doc.Environments {
		for j := range b.doc.Environments[i].Variables {
			v := &b.doc.Environments[i].Variables[j]
			walk(reflect.ValueOf(&v.Value).Elem(), "Value")
		}
		walk(reflect.ValueOf(&b.doc.Environments[i].Name).Elem(), "Name")
	}
	variables := b.doc.Environments
	b.doc.Environments = nil
	walk(reflect.ValueOf(&b.doc).Elem(), "")
	b.doc.Environments = variables
	if scrubError != nil {
		return errors.New("external project exceeds its credential redaction output budget")
	}
	return nil
}

type externalLimitedText struct {
	text      strings.Builder
	remaining *int
}

func (w *externalLimitedText) Write(value []byte) (int, error) {
	if len(value) > *w.remaining {
		return 0, io.ErrShortWrite
	}
	*w.remaining -= len(value)
	return w.text.Write(value)
}

func externalSortedNames(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
