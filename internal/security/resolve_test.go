package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"relay/internal/domain"
)

func TestResolveTextPublicAndSecretWithoutRecursion(t *testing.T) {
	s := NewMemoryStore()
	if err := s.Set(t.Context(), "vault/ref", "real-token-123"); err != nil {
		t.Fatal(err)
	}
	env := domain.Environment{Variables: []domain.Variable{
		{Name: "host", Value: "example.com"},
		{Name: "literal", Value: "{{host}}"},
		{Name: "token", Secret: true, SecretRef: "vault/ref", Value: "must-ignore-secret-model-value"},
	}}
	got, err := ResolveText(t.Context(), "https://{{host}}/{{literal}}?token={{token}}", env, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "https://example.com/{{host}}?token=real-token-123" {
		t.Fatalf("value %q", got.Value)
	}
	if got.Redacted != "https://example.com/{{host}}?token=[REDACTED]" {
		t.Fatalf("redacted %q", got.Redacted)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "real-token-123") {
		t.Fatalf("marshaled wire value: %s %v", encoded, err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if logged := fmt.Sprintf(format, got); strings.Contains(logged, "real-token-123") {
			t.Fatalf("formatted wire value: %s", logged)
		}
	}
}

type leakingStore struct{}

func (leakingStore) Set(context.Context, string, string) error { return nil }
func (leakingStore) Delete(context.Context, string) error      { return nil }
func (leakingStore) Get(context.Context, string) (string, error) {
	return "secret-value", errors.New("failed secret-ref secret-value")
}

func TestResolveErrorsNeverExposeValuesOrReferences(t *testing.T) {
	for _, test := range []struct {
		name, body string
		variables  []domain.Variable
		store      Store
		want       string
	}{
		{name: "missing", body: "{{missing}}", want: `variable "missing" is not defined`},
		{name: "missing reference", body: "{{token}}", variables: []domain.Variable{{Name: "token", Secret: true, Value: "secret-value"}}, want: "no secret reference"},
		{name: "backend leaks", body: "{{token}}", variables: []domain.Variable{{Name: "token", Secret: true, SecretRef: "secret-ref"}}, store: leakingStore{}, want: "secret store unavailable"},
		{name: "no backend", body: "{{token}}", variables: []domain.Variable{{Name: "token", Secret: true, SecretRef: "secret-ref"}}, want: "secret store unavailable"},
		{name: "missing secret", body: "{{token}}", variables: []domain.Variable{{Name: "token", Secret: true, SecretRef: "secret-ref"}}, store: NewMemoryStore(), want: "secret not found"},
		{name: "invalid name", body: "{{secret-value\n}}", want: "invalid variable placeholder"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := ResolveText(t.Context(), test.body, domain.Environment{Variables: test.variables}, test.store)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unexpected error %v", err)
			}
			if result != (ResolvedText{}) || strings.Contains(err.Error(), "secret-ref") || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("secret exposed: %+v %v", result, err)
			}
		})
	}
}

func TestResolveJSONEscapesAndPreservesInt64(t *testing.T) {
	secret := "line one\nline two\"\\{\"inject\":true}"
	s := NewMemoryStore()
	if err := s.Set(t.Context(), "ref", secret); err != nil {
		t.Fatal(err)
	}
	env := domain.Environment{Variables: []domain.Variable{{Name: "secret", Secret: true, SecretRef: "ref"}, {Name: "number", Value: "123"}}}
	result, err := ResolveJSON(t.Context(), `{"count":9223372036854775807,"min":-9223372036854775808,"number":"{{number}}","nested":["x{{secret}}y"],"exponent":1.234e+30}`, env, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Value, `9223372036854775807`) || !strings.Contains(result.Value, `-9223372036854775808`) || !strings.Contains(result.Value, `1.234e+30`) {
		t.Fatalf("numbers lost precision: %s", result.Value)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.Value), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["nested"].([]any)[0] != "x"+secret+"y" || decoded["number"] != "123" {
		t.Fatalf("incorrect substitution %#v", decoded)
	}
	if strings.Contains(result.Redacted, "line one") || !strings.Contains(result.Redacted, RedactedValue) || !json.Valid([]byte(result.Redacted)) {
		t.Fatalf("unsafe redacted JSON %s", result.Redacted)
	}
}

func TestResolveJSONRejectsUnsafeTemplates(t *testing.T) {
	for _, body := range []string{`{"count":{{number}}}`, `{"{{key}}":"value"}`, `{"value":"secret-value",}`, `{} {}`, strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)} {
		result, err := ResolveJSON(t.Context(), body, domain.Environment{}, nil)
		if err == nil || result != (ResolvedText{}) {
			t.Fatalf("accepted unsafe template %s", body)
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("echoed JSON error %v", err)
		}
	}
}

func TestResolveBoundsAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ResolveText(ctx, "{{anything}}", domain.Environment{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := ResolveText(t.Context(), strings.Repeat("x", MaxTemplateBytes+1), domain.Environment{}, nil); err == nil {
		t.Fatal("accepted oversized template")
	}
	env := domain.Environment{Variables: []domain.Variable{{Name: "huge", Value: strings.Repeat("x", MaxTemplateBytes/2+1)}}}
	if _, err := ResolveJSON(t.Context(), `["{{huge}}","{{huge}}"]`, env, nil); err == nil {
		t.Fatal("accepted oversized result")
	}
	for _, name := range []string{"", "token\n", "token value", "1bad", strings.Repeat("a", 129)} {
		if err := ValidateVariableName(name); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
	for _, name := range []string{"token", "_token", "base.url", "token-1"} {
		if err := ValidateVariableName(name); err != nil {
			t.Fatalf("rejected name %q", name)
		}
	}
}

func TestResolveJSONRejectsDenseTreesBeforeResolution(t *testing.T) {
	const canary = "dense-json-source-canary"
	for _, body := range []string{
		`["` + canary + `",` + strings.Repeat("0,", maxJSONStructure) + `"{{missing}}"]`,
		`{"x":[` + strings.Repeat("{},", maxJSONStructure/2) + `"{{missing}}"]}`,
	} {
		if len(body) >= MaxTemplateBytes {
			t.Fatal("fixture must fit the byte budget")
		}
		result, err := ResolveJSON(t.Context(), body, domain.Environment{}, nil)
		if result != (ResolvedText{}) || err == nil || !strings.Contains(err.Error(), "structural limit") || strings.Contains(err.Error(), canary) {
			t.Fatalf("dense tree did not fail safely before resolving variables: %v", err)
		}
	}
	// Punctuation in a large ordinary string does not create JSON nodes.
	body := `{"text":"` + strings.Repeat("[{},:]", maxJSONStructure) + `"}`
	if _, err := ResolveJSON(t.Context(), body, domain.Environment{}, nil); err != nil {
		t.Fatal("string punctuation counted as structure:", err)
	}
}
