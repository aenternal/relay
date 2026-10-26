package jsoncodec

import (
	"encoding/json"
	"fmt"

	"relay/internal/protobuf/registry"

	"google.golang.org/protobuf/reflect/protoreflect"
)

const MaxTemplateBytes = 256 << 10

// Template creates an example without copying source defaults, comments or any
// request values. It chooses only one real oneof alternative, leaves lists/maps
// empty, and stops optional recursive edges. Required recursive graphs fail.
func Template(reg *registry.Registry, descriptor protoreflect.MessageDescriptor) ([]byte, error) {
	if reg == nil || descriptor == nil {
		return nil, fmt.Errorf("generate Protobuf template: select an imported message")
	}
	state := templateState{active: make(map[protoreflect.FullName]bool)}
	value, err := state.message(descriptor, 0)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("generate Protobuf template: invalid schema value")
	}
	if len(data) > MaxTemplateBytes {
		return nil, fmt.Errorf("Protobuf template exceeds the 256 KiB limit")
	}
	if _, err := Decode(reg, descriptor, string(data), StrictJSON); err != nil {
		return nil, fmt.Errorf("cannot create a finite valid template for this message: %w", err)
	}
	return data, nil
}

type templateState struct {
	active map[protoreflect.FullName]bool
	nodes  int
	budget int
}

func (s *templateState) message(m protoreflect.MessageDescriptor, depth int) (any, error) {
	if depth > 12 {
		return nil, fmt.Errorf("Protobuf template exceeds the depth limit")
	}
	switch m.FullName() {
	case "google.protobuf.Timestamp":
		return "1970-01-01T00:00:00Z", nil
	case "google.protobuf.Duration":
		return "0s", nil
	case "google.protobuf.FieldMask", "google.protobuf.StringValue", "google.protobuf.BytesValue":
		return "", nil
	case "google.protobuf.Int64Value", "google.protobuf.UInt64Value":
		return "0", nil
	case "google.protobuf.DoubleValue", "google.protobuf.FloatValue", "google.protobuf.Int32Value", "google.protobuf.UInt32Value":
		return 0, nil
	case "google.protobuf.BoolValue":
		return false, nil
	case "google.protobuf.Value":
		return nil, nil
	case "google.protobuf.ListValue":
		return []any{}, nil
	case "google.protobuf.Struct", "google.protobuf.Any", "google.protobuf.Empty":
		return map[string]any{}, nil
	}
	s.active[m.FullName()] = true
	defer delete(s.active, m.FullName())
	result := make(map[string]any)
	oneofs := make(map[protoreflect.FullName]bool)
	for i := 0; i < m.Fields().Len(); i++ {
		f := m.Fields().Get(i)
		s.nodes++
		// JSON escaping can expand a custom json_name by up to six bytes per
		// input byte. Reserve indentation and scalar syntax before marshaling.
		s.budget += len(f.JSONName())*6 + 128
		if s.budget > MaxTemplateBytes {
			return nil, fmt.Errorf("Protobuf template exceeds the 256 KiB limit")
		}
		if s.nodes > 2048 {
			return nil, fmt.Errorf("Protobuf template exceeds the field limit")
		}
		if oneof := f.ContainingOneof(); oneof != nil && !oneof.IsSynthetic() {
			if oneofs[oneof.FullName()] {
				continue
			}
			oneofs[oneof.FullName()] = true
		}
		var value any
		switch {
		case f.IsMap():
			value = map[string]any{}
		case f.IsList():
			value = []any{}
		case f.Message() != nil:
			if s.active[f.Message().FullName()] || depth >= 12 {
				if f.Cardinality() == protoreflect.Required {
					return nil, fmt.Errorf("required recursive message has no finite template")
				}
				continue
			}
			var err error
			value, err = s.message(f.Message(), depth+1)
			if err != nil {
				return nil, err
			}
		case f.Enum() != nil:
			if f.Enum().FullName() == "google.protobuf.NullValue" {
				value = nil
			} else {
				value = string(f.Enum().Values().Get(0).Name())
			}
		default:
			switch f.Kind() {
			case protoreflect.StringKind, protoreflect.BytesKind:
				value = ""
			case protoreflect.BoolKind:
				value = false
			case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
				value = "0"
			default:
				value = 0
			}
		}
		result[f.JSONName()] = value
	}
	return result, nil
}
