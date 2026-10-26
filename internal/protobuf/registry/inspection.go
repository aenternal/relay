package registry

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

type FileInfo struct {
	Name    string   `json:"name"`
	Package string   `json:"package"`
	Syntax  string   `json:"syntax"`
	Imports []string `json:"imports"`
}

type ServiceInfo struct {
	Name    string       `json:"name"`
	File    string       `json:"file"`
	Comment string       `json:"comment"`
	Methods []MethodInfo `json:"methods"`
}

type EnumValueInfo struct {
	Name   string `json:"name"`
	Number int32  `json:"number"`
}

type FieldInfo struct {
	Name       string          `json:"name"`
	JSONName   string          `json:"jsonName"`
	Number     int32           `json:"number"`
	Kind       string          `json:"kind"`
	Type       string          `json:"type"`
	Repeated   bool            `json:"repeated"`
	Required   bool            `json:"required"`
	Presence   bool            `json:"presence"`
	Map        bool            `json:"map"`
	MapKey     string          `json:"mapKey"`
	MapValue   string          `json:"mapValue"`
	Oneof      string          `json:"oneof"`
	Comment    string          `json:"comment"`
	EnumValues []EnumValueInfo `json:"enumValues"`
}

type MessageInfo struct {
	Name    string      `json:"name"`
	File    string      `json:"file"`
	Comment string      `json:"comment"`
	Fields  []FieldInfo `json:"fields"`
}

func comment(d protoreflect.Descriptor) string {
	location := d.ParentFile().SourceLocations().ByDescriptor(d)
	return strings.TrimSpace(location.LeadingComments + location.TrailingComments)
}

func (r *Registry) Message(name string) (protoreflect.MessageDescriptor, error) {
	if r == nil || r.Files == nil {
		return nil, fmt.Errorf("resolve Protobuf message: no schema imported")
	}
	d, err := r.Files.FindDescriptorByName(protoreflect.FullName(strings.TrimPrefix(name, ".")))
	if err == nil {
		if m, ok := d.(protoreflect.MessageDescriptor); ok && !m.IsMapEntry() {
			return m, nil
		}
	}
	return nil, fmt.Errorf("resolve Protobuf message: selected type is not a message in the imported registry")
}

func (r *Registry) FilesInfo() []FileInfo {
	result := []FileInfo{}
	if r == nil || r.Files == nil {
		return result
	}
	r.Files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		item := FileInfo{Name: f.Path(), Package: string(f.Package()), Syntax: f.Syntax().String(), Imports: []string{}}
		for i := 0; i < f.Imports().Len(); i++ {
			item.Imports = append(item.Imports, f.Imports().Get(i).Path())
		}
		result = append(result, item)
		return true
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (r *Registry) Services() []ServiceInfo {
	result := []ServiceInfo{}
	if r == nil || r.Files == nil {
		return result
	}
	r.Files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		for i := 0; i < f.Services().Len(); i++ {
			s := f.Services().Get(i)
			item := ServiceInfo{Name: string(s.FullName()), File: f.Path(), Comment: comment(s), Methods: []MethodInfo{}}
			for j := 0; j < s.Methods().Len(); j++ {
				m := s.Methods().Get(j)
				item.Methods = append(item.Methods, methodInfo(m))
			}
			result = append(result, item)
		}
		return true
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func fieldType(f protoreflect.FieldDescriptor) string {
	if f.Message() != nil {
		return string(f.Message().FullName())
	}
	if f.Enum() != nil {
		return string(f.Enum().FullName())
	}
	return f.Kind().String()
}

// Messages flattens nested message names while excluding synthetic map entries.
// It describes one level of fields per message, so recursive schemas remain finite.
func (r *Registry) Messages() []MessageInfo {
	result := []MessageInfo{}
	if r == nil || r.Files == nil {
		return result
	}
	var add func(protoreflect.MessageDescriptors)
	add = func(messages protoreflect.MessageDescriptors) {
		for i := 0; i < messages.Len(); i++ {
			m := messages.Get(i)
			if m.IsMapEntry() {
				continue
			}
			item := MessageInfo{Name: string(m.FullName()), File: m.ParentFile().Path(), Comment: comment(m), Fields: []FieldInfo{}}
			for j := 0; j < m.Fields().Len(); j++ {
				f := m.Fields().Get(j)
				field := FieldInfo{Name: string(f.Name()), JSONName: f.JSONName(), Number: int32(f.Number()), Kind: f.Kind().String(), Type: fieldType(f), Repeated: f.IsList(), Required: f.Cardinality() == protoreflect.Required, Presence: f.HasPresence(), Map: f.IsMap(), Comment: comment(f), EnumValues: []EnumValueInfo{}}
				if f.IsMap() {
					field.MapKey, field.MapValue = fieldType(f.MapKey()), fieldType(f.MapValue())
				}
				if oneof := f.ContainingOneof(); oneof != nil && !oneof.IsSynthetic() {
					field.Oneof = string(oneof.Name())
				}
				if enum := f.Enum(); enum != nil {
					for k := 0; k < enum.Values().Len(); k++ {
						v := enum.Values().Get(k)
						field.EnumValues = append(field.EnumValues, EnumValueInfo{Name: string(v.Name()), Number: int32(v.Number())})
					}
				}
				item.Fields = append(item.Fields, field)
			}
			result = append(result, item)
			add(m.Messages())
		}
	}
	r.Files.RangeFiles(func(f protoreflect.FileDescriptor) bool { add(f.Messages()); return true })
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// DescriptorSet exports the full dependency closure and source comments. File
// ordering and wire serialization are deterministic for checksums and snapshots.
func (r *Registry) DescriptorSet() ([]byte, error) {
	if r == nil || r.Files == nil {
		return nil, fmt.Errorf("export descriptor set: no schema imported")
	}
	set := &descriptorpb.FileDescriptorSet{}
	r.Files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		set.File = append(set.File, protodesc.ToFileDescriptorProto(f))
		return true
	})
	sort.Slice(set.File, func(i, j int) bool { return set.File[i].GetName() < set.File[j].GetName() })
	return proto.MarshalOptions{Deterministic: true}.Marshal(set)
}
