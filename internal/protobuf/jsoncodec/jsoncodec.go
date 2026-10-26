// Package jsoncodec converts editor text into real dynamic Protobuf messages.
package jsoncodec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"unicode/utf8"

	"relay/internal/protobuf/registry"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type Format string

const (
	StrictJSON Format = "json"
	JSON5Like  Format = "json5"
	// MaxInputBytes and MaxDepth bound editor normalization and recursive parsing.
	MaxInputBytes  = 4 << 20
	MaxDepth       = 100
	MaxOutputBytes = 8 << 20
)

// Decode validates the selected input descriptor using official ProtoJSON rules.
// Unknown fields and missing proto2 required fields are errors.
func Decode(reg *registry.Registry, descriptor protoreflect.MessageDescriptor, body string, format Format) (*dynamicpb.Message, error) {
	if reg == nil || reg.Types == nil {
		return nil, fmt.Errorf("decode ProtoJSON: no schema imported")
	}
	if descriptor == nil {
		return nil, fmt.Errorf("decode ProtoJSON: no message descriptor selected")
	}
	normalized, positions, err := normalize(body, format)
	if err != nil {
		return nil, err
	}
	message := dynamicpb.NewMessage(descriptor)
	if err := (protojson.UnmarshalOptions{Resolver: reg.Types, DiscardUnknown: false, RecursionLimit: MaxDepth}).Unmarshal(normalized, message); err != nil {
		return nil, protobufError(reg, body, normalized, positions, descriptor, err)
	}
	return message, nil
}

// Encode preserves ProtoJSON's 64-bit integer strings and dynamic Any resolution.
func Encode(reg *registry.Registry, message proto.Message) ([]byte, error) {
	if reg == nil || reg.Types == nil {
		return nil, fmt.Errorf("encode ProtoJSON: no schema imported")
	}
	if message == nil {
		return nil, fmt.Errorf("encode ProtoJSON: nil message")
	}
	body, err := (protojson.MarshalOptions{Resolver: reg.Types}).Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode ProtoJSON %s: %s", message.ProtoReflect().Descriptor().FullName(), safeDiagnostic(reg, err))
	}
	if len(body) > MaxOutputBytes {
		return nil, fmt.Errorf("encoded ProtoJSON exceeds the 8 MiB limit")
	}
	// A deeply nested, dense 4 MiB input can expand by hundreds of MiB with
	// indentation. Bound that expansion before allocating the pretty preview.
	delimiters := bytes.Count(body, []byte(",")) + bytes.Count(body, []byte("{")) + bytes.Count(body, []byte("["))
	if len(body)+delimiters*(2*MaxDepth+4) <= MaxOutputBytes {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err == nil {
			body = pretty.Bytes()
		}
	}
	return body, nil
}

var protoPosition = regexp.MustCompile(`\(line ([0-9]+):([0-9]+)\)`)

func protobufError(reg *registry.Registry, source string, normalized []byte, positions []int, descriptor protoreflect.MessageDescriptor, err error) error {
	// protojson reports positions in normalized JSON. Map them back to editor text,
	// including comments and quote conversions removed by normalization.
	match := protoPosition.FindStringSubmatch(err.Error())
	if match != nil {
		line, _ := strconv.Atoi(match[1])
		column, _ := strconv.Atoi(match[2])
		offset, current := 0, 1
		for offset < len(normalized) && current < line {
			if normalized[offset] == '\n' {
				current++
			}
			offset++
		}
		for c := 1; c < column && offset < len(normalized); c++ {
			_, size := utf8.DecodeRune(normalized[offset:])
			offset += size
		}
		if offset >= 0 && offset < len(positions) {
			originalLine, originalColumn := lineColumn(source, positions[offset])
			return &ValidationError{Line: originalLine, Column: originalColumn, Path: fieldPath(normalized, descriptor, offset), Message: safeDiagnostic(reg, err)}
		}
	}
	return &ValidationError{Path: "$", Message: safeDiagnostic(reg, err)}
}
