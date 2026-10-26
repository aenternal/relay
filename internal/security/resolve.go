package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"relay/internal/domain"
)

const (
	MaxTemplateBytes = 4 << 20
	maxVariables     = 4096
	maxSubstitutions = 16384
	maxJSONDepth     = 64
	maxJSONStructure = 100000
	RedactedValue    = "[REDACTED]"
)

// ResolvedText carries the wire value separately from its safe preview. Only
// Redacted may enter history or diagnostic UI; Value exists for execution.
type ResolvedText struct {
	Value    string `json:"-"`
	Redacted string `json:"redacted"`
}

// Formatting the result for diagnostics must use the same safe representation
// as JSON serialization. Execution code accesses Value explicitly.
func (r ResolvedText) String() string   { return r.Redacted }
func (r ResolvedText) GoString() string { return r.Redacted }

// ValidateVariableName restricts names so they are safe to include in errors.
func ValidateVariableName(name string) error {
	if name == "" || len(name) > 128 {
		return errors.New("invalid variable name")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' ||
			(i > 0 && ((c >= '0' && c <= '9') || c == '.' || c == '-')) {
			continue
		}
		return errors.New("invalid variable name")
	}
	return nil
}

type resolver struct {
	ctx               context.Context
	variables         map[string]domain.Variable
	store             Store
	resolved          map[string]ResolvedText
	substitutions     int
	jsonValueBytes    int
	jsonRedactedBytes int
}

func newResolver(ctx context.Context, env domain.Environment, store Store) (*resolver, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(env.Variables) > maxVariables {
		return nil, errors.New("too many environment variables")
	}
	r := &resolver{ctx: ctx, store: store, variables: make(map[string]domain.Variable, len(env.Variables)), resolved: make(map[string]ResolvedText)}
	for _, variable := range env.Variables {
		if err := ValidateVariableName(variable.Name); err != nil {
			return nil, err
		}
		if _, exists := r.variables[variable.Name]; exists {
			return nil, fmt.Errorf("duplicate variable %q", variable.Name)
		}
		r.variables[variable.Name] = variable
	}
	return r, nil
}

// ResolveText performs one substitution pass. Inserted variable contents are
// never parsed again, so a value containing {{other}} remains literal text.
func ResolveText(ctx context.Context, template string, env domain.Environment, store Store) (ResolvedText, error) {
	if len(template) > MaxTemplateBytes {
		return ResolvedText{}, errors.New("template exceeds 4 MiB limit")
	}
	r, err := newResolver(ctx, env, store)
	if err != nil {
		return ResolvedText{}, err
	}
	return r.text(template)
}

func (r *resolver) text(template string) (ResolvedText, error) {
	var value, redacted strings.Builder
	for {
		if err := r.ctx.Err(); err != nil {
			return ResolvedText{}, err
		}
		start := strings.Index(template, "{{")
		if start < 0 {
			if value.Len()+len(template) > MaxTemplateBytes || redacted.Len()+len(template) > MaxTemplateBytes {
				return ResolvedText{}, errors.New("resolved template exceeds 4 MiB limit")
			}
			value.WriteString(template)
			redacted.WriteString(template)
			return ResolvedText{Value: value.String(), Redacted: redacted.String()}, nil
		}
		end := strings.Index(template[start+2:], "}}")
		if end < 0 {
			return ResolvedText{}, errors.New("unterminated variable placeholder")
		}
		end += start + 2
		name := template[start+2 : end]
		if err := ValidateVariableName(name); err != nil {
			return ResolvedText{}, errors.New("invalid variable placeholder")
		}
		r.substitutions++
		if r.substitutions > maxSubstitutions {
			return ResolvedText{}, errors.New("too many variable placeholders")
		}
		resolved, err := r.variable(name)
		if err != nil {
			return ResolvedText{}, err
		}
		if value.Len()+start+len(resolved.Value) > MaxTemplateBytes || redacted.Len()+start+len(resolved.Redacted) > MaxTemplateBytes {
			return ResolvedText{}, errors.New("resolved template exceeds 4 MiB limit")
		}
		value.WriteString(template[:start])
		redacted.WriteString(template[:start])
		value.WriteString(resolved.Value)
		redacted.WriteString(resolved.Redacted)
		template = template[end+2:]
	}
}

func (r *resolver) variable(name string) (ResolvedText, error) {
	if result, ok := r.resolved[name]; ok {
		return result, nil
	}
	variable, ok := r.variables[name]
	if !ok {
		return ResolvedText{}, fmt.Errorf("variable %q is not defined", name)
	}
	result := ResolvedText{Value: variable.Value, Redacted: variable.Value}
	if variable.Secret {
		if variable.SecretRef == "" {
			return ResolvedText{}, fmt.Errorf("variable %q has no secret reference", name)
		}
		if r.store == nil {
			return ResolvedText{}, fmt.Errorf("variable %q: %w", name, ErrUnavailable)
		}
		value, err := r.store.Get(r.ctx, variable.SecretRef)
		if err != nil {
			return ResolvedText{}, fmt.Errorf("variable %q: %w", name, safeStoreError(err))
		}
		if err := r.ctx.Err(); err != nil {
			return ResolvedText{}, err
		}
		result = ResolvedText{Value: value, Redacted: RedactedValue}
	}
	if len(result.Value) > MaxTemplateBytes || len(result.Redacted) > MaxTemplateBytes {
		return ResolvedText{}, fmt.Errorf("variable %q exceeds 4 MiB limit", name)
	}
	r.resolved[name] = result
	return result, nil
}

// ResolveJSON substitutes only decoded JSON strings and marshals them again,
// preserving escaping and number lexemes. Bare {{name}} is deliberately invalid
// JSON; variable values cannot change a field's type or inject JSON structure.
func ResolveJSON(ctx context.Context, body string, env domain.Environment, store Store) (ResolvedText, error) {
	if len(body) > MaxTemplateBytes {
		return ResolvedText{}, errors.New("JSON template exceeds 4 MiB limit")
	}
	if err := checkJSONDepth(body); err != nil {
		return ResolvedText{}, err
	}
	r, err := newResolver(ctx, env, store)
	if err != nil {
		return ResolvedText{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var source any
	if err := decoder.Decode(&source); err != nil {
		return ResolvedText{}, errors.New("invalid JSON template; placeholders must be inside strings")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ResolvedText{}, errors.New("JSON template must contain one value")
	}
	value, redacted, err := r.jsonValue(source)
	if err != nil {
		return ResolvedText{}, err
	}
	valueBytes, err := json.Marshal(value)
	if err != nil {
		return ResolvedText{}, errors.New("cannot encode resolved JSON")
	}
	redactedBytes, err := json.Marshal(redacted)
	if err != nil {
		return ResolvedText{}, errors.New("cannot encode redacted JSON")
	}
	if len(valueBytes) > MaxTemplateBytes || len(redactedBytes) > MaxTemplateBytes {
		return ResolvedText{}, errors.New("resolved JSON exceeds 4 MiB limit")
	}
	return ResolvedText{Value: string(valueBytes), Redacted: string(redactedBytes)}, nil
}

func checkJSONDepth(body string) error {
	depth, structure := 0, 0
	inString, escaped := false, false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[', '{':
			depth++
			structure++
			if depth > maxJSONDepth {
				return errors.New("JSON template exceeds nesting limit")
			}
		case ']', '}':
			depth--
		case ',', ':':
			structure++
		}
		// A short flat array can otherwise allocate millions of interface values
		// in Decode, followed by both resolved copies. Count structural bytes
		// outside strings before building any of those trees.
		if structure > maxJSONStructure {
			return errors.New("JSON template exceeds structural limit")
		}
	}
	return nil
}

func (r *resolver) jsonValue(source any) (any, any, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, nil, err
	}
	switch source := source.(type) {
	case string:
		result, err := r.text(source)
		if err != nil {
			return nil, nil, err
		}
		r.jsonValueBytes += len(result.Value)
		r.jsonRedactedBytes += len(result.Redacted)
		if r.jsonValueBytes > MaxTemplateBytes || r.jsonRedactedBytes > MaxTemplateBytes {
			return nil, nil, errors.New("resolved JSON exceeds 4 MiB limit")
		}
		return result.Value, result.Redacted, err
	case []any:
		value, redacted := make([]any, len(source)), make([]any, len(source))
		for i, item := range source {
			var err error
			value[i], redacted[i], err = r.jsonValue(item)
			if err != nil {
				return nil, nil, err
			}
		}
		return value, redacted, nil
	case map[string]any:
		value, redacted := make(map[string]any, len(source)), make(map[string]any, len(source))
		for key, item := range source {
			// Dynamic object keys can collide and would make redacted history
			// differ structurally from the request. Keep keys explicit.
			if strings.Contains(key, "{{") {
				return nil, nil, errors.New("variable placeholders are not allowed in JSON object keys")
			}
			var err error
			value[key], redacted[key], err = r.jsonValue(item)
			if err != nil {
				return nil, nil, err
			}
		}
		return value, redacted, nil
	default:
		return source, source, nil
	}
}
