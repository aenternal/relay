package jsoncodec

import (
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// ValidationError exposes editor coordinates and a schema-derived field path.
// Map keys, unknown keys and values are never copied into Message or Path.
type ValidationError struct {
	Line    int
	Column  int
	Path    string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("ProtoJSON at %d:%d (%s): %s", e.Line, e.Column, e.Path, e.Message)
	}
	return fmt.Sprintf("ProtoJSON %s: %s", e.Path, e.Message)
}

// fieldPath locates the failing JSON token with a streaming scanner. Only
// descriptor-verified names become path segments. Dynamic map keys use [*].
func fieldPath(body []byte, descriptor protoreflect.MessageDescriptor, offset int) string {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	finder := pathFinder{decoder: decoder, offset: int64(offset), found: "$"}
	finder.value(descriptor, nil, "$", 0)
	return finder.found
}

type pathFinder struct {
	decoder *json.Decoder
	offset  int64
	found   string
}

func (p *pathFinder) token(path string) (json.Token, bool) {
	start := p.decoder.InputOffset()
	token, err := p.decoder.Token()
	if err != nil {
		return nil, false
	}
	if p.offset >= start && p.offset < p.decoder.InputOffset() {
		p.found = path
	}
	return token, true
}

func (p *pathFinder) value(message protoreflect.MessageDescriptor, field protoreflect.FieldDescriptor, path string, depth int) {
	if depth > MaxDepth {
		return
	}
	token, ok := p.token(path)
	if !ok {
		return
	}
	switch token {
	case json.Delim('{'):
		for p.decoder.More() {
			keyStart := p.decoder.InputOffset()
			key, ok := p.token(path)
			if !ok {
				return
			}
			name, _ := key.(string)
			var child protoreflect.FieldDescriptor
			childPath := path
			if field != nil && field.IsMap() {
				child, childPath = field.MapValue(), path+"[*]"
			} else if message != nil {
				child = message.Fields().ByJSONName(name)
				if child == nil {
					child = message.Fields().ByName(protoreflect.Name(name))
				}
				if child != nil {
					childPath += "." + child.JSONName()
				}
			}
			// A protojson diagnostic can point at the key instead of its value.
			if p.offset >= keyStart && p.offset < p.decoder.InputOffset() {
				p.found = childPath
			}
			var nested protoreflect.MessageDescriptor
			if child != nil {
				nested = child.Message()
			}
			p.value(nested, child, childPath, depth+1)
		}
		p.token(path)
	case json.Delim('['):
		for i := 0; p.decoder.More(); i++ {
			p.value(message, nil, fmt.Sprintf("%s[%d]", path, i), depth+1)
		}
		p.token(path)
	}
}
