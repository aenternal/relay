package importer

import (
	"bytes"
	"errors"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

const MaxDescriptorWork = 100_000

// MaxSourceWork bounds parser token/comment and line-index allocations before
// compilation. Source bytes alone do not bound the number of AST nodes.
const MaxSourceWork = 100_000

type sourceBudget struct{ work, lines int }

func (b *sourceBudget) add(data []byte) error {
	lines := bytes.Count(data, []byte{'\n'})
	if lines > MaxSourceWork-b.lines {
		return errors.New("Protobuf sources exceed the 100000 line limit")
	}
	b.lines += lines
	for i := 0; i < len(data); {
		c := data[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		b.work++
		if b.work > MaxSourceWork {
			return errors.New("Protobuf sources exceed the 100000 lexical token/comment limit")
		}
		if c == '/' && i+1 < len(data) {
			if data[i+1] == '/' {
				i += 2
				for i < len(data) && data[i] != '\n' {
					i++
				}
				continue
			}
			if data[i+1] == '*' {
				i += 2
				for i < len(data) && !(data[i] == '*' && i+1 < len(data) && data[i+1] == '/') {
					i++
				}
				if i < len(data) {
					i += 2
				}
				continue
			}
		}
		i++
		if c == '"' || c == '\'' {
			for i < len(data) {
				next := data[i]
				i++
				if next == '\\' && i < len(data) {
					i++
				} else if next == c {
					break
				}
			}
		} else if sourceWord(c) {
			for i < len(data) && sourceWord(data[i]) {
				i++
			}
		}
	}
	return nil
}

func sourceWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// DescriptorBudget rejects descriptor allocation bombs before proto.Unmarshal.
// Its zero value is ready to use; AddFile calls share a single work budget for
// the complete reflection response, including repeated files.
type DescriptorBudget struct{ work int }

func (b *DescriptorBudget) consume(amount int) error {
	if amount < 0 || amount > MaxDescriptorWork-b.work {
		return errors.New("Protobuf descriptors exceed the 100000 structural field/node limit")
	}
	b.work += amount
	return nil
}

func (b *DescriptorBudget) AddFile(data []byte) error {
	if len(data) > MaxTotalBytes {
		return errors.New("Protobuf descriptor file exceeds 16 MiB")
	}
	return b.scan(data, (&descriptorpb.FileDescriptorProto{}).ProtoReflect().Descriptor(), 0)
}

func (b *DescriptorBudget) scan(data []byte, message protoreflect.MessageDescriptor, depth int) error {
	if depth > 100 {
		return errors.New("Protobuf descriptors exceed the recursion depth limit")
	}
	if err := b.consume(1); err != nil {
		return err
	}
	for len(data) > 0 {
		if err := b.consume(1); err != nil {
			return err
		}
		number, wire, tagBytes := protowire.ConsumeTag(data)
		if tagBytes < 0 || number <= 0 || wire == protowire.StartGroupType || wire == protowire.EndGroupType {
			return errors.New("malformed Protobuf descriptor wire")
		}
		data = data[tagBytes:]
		if wire == protowire.BytesType {
			value, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return errors.New("malformed Protobuf descriptor wire")
			}
			if field := message.Fields().ByNumber(protoreflect.FieldNumber(number)); field != nil {
				if field.Kind() == protoreflect.MessageKind {
					if err := b.scan(value, field.Message(), depth+1); err != nil {
						return err
					}
				} else if field.IsList() && field.Kind() != protoreflect.StringKind && field.Kind() != protoreflect.BytesKind {
					// A packed path/span can otherwise allocate millions of integers.
					// Counting bytes is a conservative bound for varint elements.
					if err := b.consume(len(value)); err != nil {
						return err
					}
				}
			}
			data = data[n:]
			continue
		}
		n := protowire.ConsumeFieldValue(number, wire, data)
		if n < 0 {
			return errors.New("malformed Protobuf descriptor wire")
		}
		data = data[n:]
	}
	return nil
}

// ValidateDescriptorSetBudget applies the same pre-allocation guard to compiled
// source artifacts before persistence and to descriptor sets received on disk.
func ValidateDescriptorSetBudget(data []byte) error {
	if len(data) == 0 || len(data) > MaxTotalBytes {
		return errors.New("Protobuf descriptor set must contain between 1 byte and 16 MiB")
	}
	var budget DescriptorBudget
	return budget.scan(data, (&descriptorpb.FileDescriptorSet{}).ProtoReflect().Descriptor(), 0)
}
