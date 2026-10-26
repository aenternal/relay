package project

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

func externalFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../testdata/project/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPostmanHTTPCollectionPreservesStructureAndExcludesCredentials(t *testing.T) {
	doc, warnings, err := ParsePostman(externalFixture(t, "postman-v21.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Collections) != 1 || len(doc.Folders) != 2 || len(doc.Requests) != 3 || len(doc.Environments) != 1 {
		t.Fatalf("incomplete import: %+v", Statistics(doc))
	}
	if doc.Folders[1].ParentID != doc.Folders[0].ID || doc.Requests[0].FolderID != doc.Folders[1].ID {
		t.Fatal("nested folder ownership was lost")
	}
	h := doc.Requests[0].HTTP
	if h.Method != "POST" || h.URL != "{{base_url}}/accounts/:id" || h.BodyKind != "json" || h.Auth.Type != "none" {
		t.Fatalf("HTTP semantics changed: %+v", h)
	}
	if len(h.Query) != 3 || h.Query[0].Name != "tag" || h.Query[1].Name != "tag" || h.Query[1].Enabled || !h.Query[2].Enabled {
		t.Fatalf("query order or disabled state changed: %+v", h.Query)
	}
	if len(h.Headers) != 3 || h.Headers[0].Value != "first" || h.Headers[1].Value != "second" || h.Headers[1].Enabled || h.PathParams[0].Value != "42" {
		t.Fatal("repeated headers, disabled state, or path variables were lost")
	}
	if doc.Requests[1].HTTP.BodyKind != "form" || doc.Requests[1].HTTP.Form[1].Enabled || len(doc.Requests[1].HTTP.Query) != 2 {
		t.Fatal("form or URL query conversion failed")
	}
	if doc.Requests[2].HTTP.BodyKind != "multipart" || len(doc.Requests[2].HTTP.Multipart) != 1 {
		t.Fatal("multipart text/file conversion failed")
	}
	assertExternalClean(t, doc, warnings, "postman-auth-canary", "postman-env-canary", "postman-user-canary", "postman-password-canary", "postman-query-canary", "postman-header-canary", "/private/source-canary.txt")
	assertExternalWarnings(t, warnings, "scripts-omitted", "secret-omitted", "file-body-omitted", "structured-query-used")
	if !strings.Contains(h.Body, "{{region}}") || !strings.Contains(h.Body, "{{import_secret_") {
		t.Fatal("simple references or copies of omitted credentials were not normalized")
	}
	if _, _, err := Prepare(context.Background(), doc, Options{IncludeBodies: true}); err != nil {
		t.Fatalf("import does not satisfy native/storage invariants: %v", err)
	}
	without, _, err := Prepare(context.Background(), doc, Options{})
	if err != nil || without.Requests[0].HTTP.Body != "" || without.Requests[1].HTTP.Form[0].Value != "" || without.Requests[2].HTTP.Multipart[0].Value != "" {
		t.Fatalf("bodies persisted without opt-in: %v", err)
	}
}

func TestInsomniaV4ForwardParentsEnvironmentInheritanceAndCredentials(t *testing.T) {
	doc, warnings, err := ParseInsomnia(externalFixture(t, "insomnia-v4.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Folders) != 2 || len(doc.Requests) != 3 || len(doc.Environments) != 2 {
		t.Fatalf("incomplete import: %+v", Statistics(doc))
	}
	if doc.Folders[1].ParentID != doc.Folders[0].ID || doc.Requests[0].FolderID != doc.Folders[1].ID {
		t.Fatal("forward folder references were not resolved")
	}
	h := doc.Requests[0].HTTP
	if h.URL != "{{base_url}}/items" || h.BodyKind != "json" || h.Auth.Token != "{{token}}" || len(h.Query) != 3 || h.Query[2].Enabled {
		t.Fatalf("request semantics changed: %+v", h)
	}
	if h.Headers[0].Value != "first" || h.Headers[1].Value != "second" || h.Headers[1].Enabled {
		t.Fatal("repeated/disabled headers were lost")
	}
	base, child := map[string]string{}, map[string]string{}
	for _, variable := range doc.Environments[0].Variables {
		base[variable.Name] = variable.Value
		if variable.Name == "token" && (!variable.Secret || variable.Value != "" || variable.SecretRef != "") {
			t.Fatal("token was not converted to an unconfigured secret")
		}
	}
	for _, variable := range doc.Environments[1].Variables {
		child[variable.Name] = variable.Value
	}
	if base["count"] != "9007199254740993" || child["count"] != base["count"] || base["base_url"] != "https://api.example.test" || child["base_url"] != "https://dev.example.test" {
		t.Fatalf("environment inheritance or number precision lost: %+v %+v", base, child)
	}
	assertExternalClean(t, doc, warnings, "insomnia-cookie-canary", "insomnia-query-canary", "insomnia-token-canary", "/private/source-canary.txt")
	assertExternalWarnings(t, warnings, "unsupported-resource", "scripts-omitted", "scoped-variables-omitted", "query-sources-combined", "file-body-omitted", "json-body-review")
	if !strings.Contains(h.Body, `"count":{{count}}`) {
		t.Fatal("JSON template requiring manual review was silently rewritten")
	}
	if _, _, err := Prepare(context.Background(), doc, Options{IncludeBodies: true}); err != nil {
		t.Fatalf("import does not satisfy native/storage invariants: %v", err)
	}
}

func assertExternalClean(t *testing.T, doc Document, warnings []Warning, forbidden ...string) {
	t.Helper()
	data, err := json.Marshal(struct {
		Doc      Document
		Warnings []Warning
	}{doc, warnings})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range forbidden {
		if strings.Contains(string(data), value) {
			t.Fatalf("import retained an omitted credential or source path: %q", value)
		}
	}
	for _, env := range doc.Environments {
		for _, variable := range env.Variables {
			if variable.Secret && (variable.Value != "" || variable.SecretRef != "") {
				t.Fatal("external secret has a literal value or nonportable keyring reference")
			}
		}
	}
}

func assertExternalWarnings(t *testing.T, warnings []Warning, expected ...string) {
	t.Helper()
	for _, code := range expected {
		found := false
		for _, warning := range warnings {
			found = found || warning.Code == code
		}
		if !found {
			t.Fatalf("missing diagnostic %s: %+v", code, warnings)
		}
	}
}

func TestExternalRejectsVersionsMalformedGraphsAndAmbiguousJSON(t *testing.T) {
	postman := string(externalFixture(t, "postman-v21.json"))
	insomnia := string(externalFixture(t, "insomnia-v4.json"))
	cases := []struct {
		name  string
		data  string
		parse func([]byte) (Document, []Warning, error)
	}{
		{"postman-v20", strings.Replace(postman, "v2.1.0", "v2.0.0", 1), ParsePostman},
		{"postman-missing-items", `{"info":{"schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"}}`, ParsePostman},
		{"postman-non-string-header", strings.Replace(postman, `"value": "first"`, `"value": {}`, 1), ParsePostman},
		{"postman-duplicate-key", `{"info":{},"info":{},"item":[]}`, ParsePostman},
		{"insomnia-v5", strings.Replace(insomnia, `"__export_format": 4`, `"__export_format": 5`, 1), ParseInsomnia},
		{"insomnia-multiple-workspaces", strings.Replace(insomnia, `"_type": "websocket_request"`, `"_type": "workspace"`, 1), ParseInsomnia},
		{"insomnia-duplicate-id", strings.Replace(insomnia, `"_id": "fld_nested"`, `"_id": "req_create"`, 1), ParseInsomnia},
		{"insomnia-orphan", strings.Replace(insomnia, `"parentId": "fld_nested"`, `"parentId": "absent"`, 1), ParseInsomnia},
		{"insomnia-folder-cycle", strings.Replace(insomnia, `"parentId": "fld_root"`, `"parentId": "fld_nested"`, 1), ParseInsomnia},
		{"insomnia-env-cycle", strings.Replace(insomnia, `"parentId": "env_base"`, `"parentId": "env_dev"`, 1), ParseInsomnia},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := test.parse([]byte(test.data)); err == nil {
				t.Fatal("invalid/unsupported import was accepted")
			}
		})
	}
}

func TestExternalTemplatesUnsupportedFeaturesAndBoundedReport(t *testing.T) {
	data := `{"info":{"name":"Bounded","schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},"auth":{"type":"oauth2","oauth2":[{"key":"accessToken","value":"do-not-copy"}]},"item":[{"name":"Dynamic","request":{"url":"https://example.test/{{$randomUUID}}","body":{"mode":"graphql","graphql":{"query":"never executed"}}}}]}`
	doc, warnings, err := ParsePostman([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Collections[0].Auth.Type != "none" || doc.Requests[0].HTTP.BodyKind != "none" || strings.Contains(doc.Requests[0].HTTP.URL, "$randomUUID") {
		t.Fatal("unsupported executable/auth/body content was retained")
	}
	assertExternalWarnings(t, warnings, "unsupported-auth", "unsupported-template", "unsupported-body")
	assertExternalClean(t, doc, warnings, "do-not-copy", "never executed")
	var items []string
	for i := 0; i < MaxWarnings+50; i++ {
		items = append(items, fmt.Sprintf(`{"name":"%d","event":[{}],"request":"https://example.test"}`, i))
	}
	bounded := `{"info":{"schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},"item":[` + strings.Join(items, ",") + `]}`
	_, warnings, err = ParsePostman([]byte(bounded))
	if err != nil || len(warnings) != MaxWarnings || warnings[len(warnings)-1].Code != "warnings-truncated" {
		t.Fatalf("unbounded diagnostic report: %d %v", len(warnings), err)
	}
	insomnia := `{"_type":"export","__export_format":4,"resources":[{"_id":"w","_type":"workspace"},{"_id":"r","_type":"request","parentId":"w","url":"https://example.test/{% response 'body' %}"}]}`
	doc, warnings, err = ParseInsomnia([]byte(insomnia))
	if err != nil {
		t.Fatal(err)
	}
	assertExternalWarnings(t, warnings, "unsupported-template")
	if strings.Contains(doc.Requests[0].HTTP.URL, "response") {
		t.Fatal("Nunjucks tag was retained")
	}
}

func TestExternalRequestAndCredentialBudgets(t *testing.T) {
	item := `{"request":"https://example.test"}`
	tooMany := `{"info":{"schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},"item":[` + strings.Repeat(item+",", MaxRequests) + item + `]}`
	if _, _, err := ParsePostman([]byte(tooMany)); err == nil {
		t.Fatal("request limit was not enforced")
	}
	var headers []string
	for i := 0; i < 1025; i++ {
		headers = append(headers, fmt.Sprintf(`{"key":"Authorization","value":"secret-%d"}`, i))
	}
	// Two individually valid header arrays exceed the credential scrub budget.
	first, second := strings.Join(headers[:1024], ","), headers[1024]
	data := `{"info":{"schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},"item":[{"request":{"url":"https://example.test","header":[` + first + `]}},{"request":{"url":"https://example.test","header":[` + second + `]}}]}`
	if _, _, err := ParsePostman([]byte(data)); err == nil {
		t.Fatal("credential redaction budget was not enforced")
	}
	expressions := `{"info":{"schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},"item":[{"request":{"url":"https://example.test","body":{"mode":"raw","raw":"` + strings.Repeat("{{$randomUUID}}", MaxRecords+1) + `"}}}]}`
	if _, _, err := ParsePostman([]byte(expressions)); err == nil {
		t.Fatal("generated variables inside a single JSON string were not bounded")
	}
}

func TestExternalAuthInheritanceAPIKeysAndOmittedCredentialCopies(t *testing.T) {
	postman := `{"info":{"schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},"variable":[{"key":"credential_ref","value":"referenced-auth-canary"}],"item":[{"request":{"url":"https://user-canary:pass-canary@example.test/path","auth":{"type":"apikey","apikey":[{"key":"key","value":"X-Key"},{"key":"value","value":"{{credential_ref}}"},{"key":"in","value":"header"}]},"body":{"mode":"raw","raw":"referenced-auth-canary user-canary pass-canary omitted-oauth-canary"}}},{"request":{"url":"https://example.test","auth":{"type":"oauth2","oauth2":[{"key":"accessToken","value":"omitted-oauth-canary"}]}}}]}`
	doc, warnings, err := ParsePostman([]byte(postman))
	if err != nil {
		t.Fatal(err)
	}
	auth := doc.Requests[0].HTTP.Auth
	if auth.Type != "api-key" || auth.KeyName != "X-Key" || auth.KeyValue != "{{credential_ref}}" || auth.In != "header" || doc.Requests[0].HTTP.URL != "https://example.test/path" {
		t.Fatalf("API key mapping or URL credentials conversion failed: %+v", auth)
	}
	assertExternalClean(t, doc, warnings, "referenced-auth-canary", "user-canary", "pass-canary", "omitted-oauth-canary")
	if _, _, err := Prepare(context.Background(), doc, Options{IncludeBodies: true}); err != nil {
		t.Fatal(err)
	}
	insomnia := `{"_type":"export","__export_format":4,"resources":[{"_id":"w","_type":"workspace","authentication":{"type":"basic","username":"insomnia-user-canary","password":"insomnia-pass-canary"}},{"_id":"r","_type":"request","parentId":"w","url":"https://example.test","authentication":{"type":"apikey","key":"api_key","value":"insomnia-key-canary","addTo":"queryParams"},"body":{"mimeType":"text/plain","text":"insomnia-user-canary insomnia-pass-canary insomnia-key-canary insomnia-disabled-canary"}},{"_id":"r2","_type":"request","parentId":"w","url":"https://example.test","authentication":{"type":"bearer","token":"insomnia-disabled-canary","disabled":true}},{"_id":"e","_type":"environment","parentId":"w","name":"Private","isPrivate":true,"data":{"ordinary":"insomnia-private-canary"}}]}`
	doc, warnings, err = ParseInsomnia([]byte(insomnia))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Collections[0].Auth.Type != "basic" || doc.Requests[0].HTTP.Auth.Type != "api-key" || doc.Requests[0].HTTP.Auth.In != "query" || doc.Requests[1].HTTP.Auth.Type != "none" {
		t.Fatal("Insomnia auth mapping changed")
	}
	assertExternalClean(t, doc, warnings, "insomnia-user-canary", "insomnia-pass-canary", "insomnia-key-canary", "insomnia-disabled-canary", "insomnia-private-canary")
	if _, _, err := Prepare(context.Background(), doc, Options{IncludeBodies: true}); err != nil {
		t.Fatal(err)
	}
}

func TestExternalRemovesEncodedCopiesOfOmittedCredentials(t *testing.T) {
	const canary = "external credential canary +/= ? 0123456789"
	variants := []string{
		canary, url.QueryEscape(canary), url.PathEscape(canary),
		base64.StdEncoding.EncodeToString([]byte(canary)), base64.RawStdEncoding.EncodeToString([]byte(canary)),
		base64.URLEncoding.EncodeToString([]byte(canary)), base64.RawURLEncoding.EncodeToString([]byte(canary)),
		hex.EncodeToString([]byte(canary)), strings.ToUpper(hex.EncodeToString([]byte(canary))),
	}
	for name, parse := range map[string]func([]byte) (Document, []Warning, error){"Postman": ParsePostman, "Insomnia": ParseInsomnia} {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(strings.Join(variants, "\n"))
			secret, _ := json.Marshal(canary)
			var data string
			if name == "Postman" {
				data = `{"info":{"schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},"item":[{"request":{"url":"https://example.test","auth":{"type":"bearer","bearer":[{"key":"token","value":` + string(secret) + `}]},"body":{"mode":"raw","raw":` + string(body) + `}}}]}`
			} else {
				data = `{"_type":"export","__export_format":4,"resources":[{"_id":"w","_type":"workspace"},{"_id":"r","_type":"request","parentId":"w","url":"https://example.test","authentication":{"type":"bearer","token":` + string(secret) + `},"body":{"mimeType":"text/plain","text":` + string(body) + `}}]}`
			}
			doc, warnings, err := parse([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			assertExternalClean(t, doc, warnings, variants...)
			// The policy pass after parsing no longer has the source credential;
			// both the preview and explicitly included body must already be safe.
			prepared, _, err := Prepare(t.Context(), doc, Options{IncludeBodies: true})
			if err != nil {
				t.Fatal(err)
			}
			assertExternalClean(t, prepared, warnings, variants...)
			if !strings.Contains(prepared.Requests[0].HTTP.Body, "{{import_secret_") {
				t.Fatal("credential copy was not replaced by a reconfigurable secret reference")
			}
		})
	}
}
