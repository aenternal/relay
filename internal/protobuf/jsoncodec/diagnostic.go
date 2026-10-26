package jsoncodec

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"relay/internal/protobuf/registry"

	"google.golang.org/protobuf/reflect/protoreflect"
)

var invalidField = regexp.MustCompile(`^invalid value for ([a-z0-9]+) field ([A-Za-z_][A-Za-z0-9_]*):`)
var invalidMapKey = regexp.MustCompile(`^invalid value for ([a-z0-9]+) key:`)
var requiredField = regexp.MustCompile(`^required field ([A-Za-z_][A-Za-z0-9_.]*) not set$`)
var duplicateField = regexp.MustCompile(`^duplicate field ("(?:[^"\\]|\\.)*")$`)
var oneofField = regexp.MustCompile(`^error parsing ("(?:[^"\\]|\\.)*"), oneof ([A-Za-z_][A-Za-z0-9_.]*) is already set$`)

// safeDiagnostic selects known diagnostic templates instead of redacting a raw
// error string. protojson can include complete values, map keys, type URLs and
// quoted nested resolver errors, so a quote-removal pass is not sufficient.
// Never return or wrap the original error, including through an Unwrap chain.
func safeDiagnostic(reg *registry.Registry, err error) string {
	raw := strings.TrimSpace(protoPosition.ReplaceAllString(err.Error(), ""))
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "proto:"))
	raw = strings.TrimSpace(strings.TrimPrefix(raw, ":"))
	if match := invalidField.FindStringSubmatch(raw); match != nil && knownKind(match[1]) {
		if knownField(reg, match[2]) {
			return fmt.Sprintf("invalid %s value for field %s: incompatible type, format or range", match[1], match[2])
		}
		return fmt.Sprintf("invalid %s field value: incompatible type, format or range", match[1])
	}
	if match := invalidMapKey.FindStringSubmatch(raw); match != nil && knownKind(match[1]) {
		return fmt.Sprintf("invalid %s map key: incompatible format or range", match[1])
	}
	if match := requiredField.FindStringSubmatch(raw); match != nil {
		if descriptor, lookupErr := reg.Files.FindDescriptorByName(protoreflect.FullName(match[1])); lookupErr == nil {
			if field, ok := descriptor.(protoreflect.FieldDescriptor); ok {
				return fmt.Sprintf("required field %s is not set", field.FullName())
			}
		}
		return "required Protobuf field is not set"
	}
	if match := duplicateField.FindStringSubmatch(raw); match != nil {
		if name := schemaField(reg, match[1]); name != "" {
			return fmt.Sprintf("duplicate field %s: each Protobuf field may appear only once", name)
		}
	}
	if match := oneofField.FindStringSubmatch(raw); match != nil {
		if name := schemaField(reg, match[1]); name != "" {
			if descriptor, lookupErr := reg.Files.FindDescriptorByName(protoreflect.FullName(match[2])); lookupErr == nil {
				if oneof, ok := descriptor.(protoreflect.OneofDescriptor); ok {
					return fmt.Sprintf("field %s: oneof %s is already set", name, oneof.FullName())
				}
			}
		}
	}
	switch {
	case strings.HasPrefix(raw, "unknown field "):
		return "unknown field: the input key does not exist in the selected message"
	case strings.HasPrefix(raw, "duplicate field "), strings.HasPrefix(raw, `duplicate "`):
		return "duplicate field: each Protobuf field may appear only once"
	case strings.HasPrefix(raw, "duplicate map key "):
		return "duplicate map key: each map key may appear only once"
	case strings.HasPrefix(raw, "error parsing ") && strings.Contains(raw, ", oneof "):
		return "oneof conflict: another field in the same oneof is already set"
	case strings.HasPrefix(raw, "unable to resolve "), strings.HasPrefix(raw, "google.protobuf.Any: unable to resolve "):
		return "unable to resolve Protobuf message or extension type in the imported schema registry"
	case strings.HasPrefix(raw, "google.protobuf.Any: unable to unmarshal "), strings.HasPrefix(raw, "error in marshaling Any.value field:"):
		return "invalid google.protobuf.Any payload for its declared message type"
	case strings.HasPrefix(raw, "@type field value is not a string:"):
		return "google.protobuf.Any @type field must be a string"
	case strings.HasPrefix(raw, "@type field contains empty value"), strings.HasPrefix(raw, "missing @type field"):
		return "google.protobuf.Any requires a nonempty @type field"
	case strings.HasPrefix(raw, `missing "value" field`):
		return "google.protobuf.Any requires a value field for this well-known type"
	case strings.HasPrefix(raw, "invalid google.protobuf.Timestamp"), strings.HasPrefix(raw, "google.protobuf.Timestamp value out of range:"):
		return "invalid google.protobuf.Timestamp value: expected an RFC 3339 timestamp in the supported range"
	case strings.HasPrefix(raw, "invalid google.protobuf.Duration"), strings.HasPrefix(raw, "google.protobuf.Duration value out of range:"):
		return "invalid google.protobuf.Duration value: expected a duration string in the supported range"
	case strings.HasPrefix(raw, "google.protobuf.FieldMask"):
		return "invalid google.protobuf.FieldMask path or representation"
	case strings.HasPrefix(raw, "invalid google.protobuf.Value"), strings.HasPrefix(raw, "google.protobuf.Value:"):
		return "invalid google.protobuf.Value representation"
	case strings.HasPrefix(raw, "syntax error"), strings.HasPrefix(raw, "unexpected token"):
		return "unexpected JSON token for the selected Protobuf field or message"
	case raw == "exceeded max recursion depth":
		return "Protobuf message exceeds the recursion depth limit"
	case raw == "unexpected EOF":
		return "unexpected end of Protobuf JSON input"
	case raw == "no support for proto1 MessageSets":
		return "proto1 MessageSets are not supported"
	default:
		return "invalid Protobuf value or message structure"
	}
}

func schemaField(reg *registry.Registry, quoted string) string {
	var name string
	if json.Unmarshal([]byte(quoted), &name) == nil && knownField(reg, name) {
		return name
	}
	return ""
}

func knownKind(kind string) bool {
	switch kind {
	case "bool", "enum", "int32", "sint32", "sfixed32", "int64", "sint64", "sfixed64", "uint32", "fixed32", "uint64", "fixed64", "float", "double", "string", "bytes":
		return true
	default:
		return false
	}
}

func knownField(reg *registry.Registry, name string) bool {
	var messagesContain func(protoreflect.MessageDescriptors) bool
	messagesContain = func(messages protoreflect.MessageDescriptors) bool {
		for i := 0; i < messages.Len(); i++ {
			message := messages.Get(i)
			fields := message.Fields()
			for j := 0; j < fields.Len(); j++ {
				field := fields.Get(j)
				if string(field.Name()) == name || field.JSONName() == name {
					return true
				}
			}
			if messagesContain(message.Messages()) {
				return true
			}
		}
		return false
	}
	found := false
	reg.Files.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		found = messagesContain(file.Messages())
		return !found
	})
	return found
}
