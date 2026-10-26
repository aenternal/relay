package jsoncodec

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// SyntaxError uses one-based editor line/column and a zero-based byte offset.
// Its message deliberately excludes source contents, which may contain secrets.
type SyntaxError struct {
	Offset  int
	Line    int
	Column  int
	Message string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("JSON at %d:%d: %s", e.Line, e.Column, e.Message)
}

// Normalize converts the supported JSON5-like subset to strict JSON without
// converting numeric tokens to float64. Hexadecimal, leading plus, NaN, Infinity,
// multiline strings, and other full JSON5 features are deliberately rejected.
func Normalize(body string, format Format) ([]byte, error) {
	result, _, err := normalize(body, format)
	return result, err
}

func normalize(body string, format Format) ([]byte, []int, error) {
	p := parser{source: body, relaxed: format == JSON5Like}
	if format != StrictJSON && format != JSON5Like {
		return nil, nil, fmt.Errorf("unsupported JSON format: select Strict JSON or JSON5-like")
	}
	if len(body) > MaxInputBytes {
		return nil, nil, p.failure(0, "input exceeds the 4 MiB limit")
	}
	if !utf8.ValidString(body) {
		return nil, nil, p.failure(0, "input is not valid UTF-8")
	}
	if err := p.value(0); err != nil {
		return nil, nil, err
	}
	if err := p.space(); err != nil {
		return nil, nil, err
	}
	if p.offset != len(body) {
		return nil, nil, p.failure(p.offset, "unexpected content after JSON value")
	}
	return p.output, p.positions, nil
}

type parser struct {
	source    string
	offset    int
	relaxed   bool
	output    []byte
	positions []int
}

func (p *parser) failure(offset int, message string) *SyntaxError {
	line, column := lineColumn(p.source, offset)
	return &SyntaxError{Offset: offset, Line: line, Column: column, Message: message}
}

func lineColumn(source string, offset int) (int, int) {
	if offset > len(source) {
		offset = len(source)
	}
	line, column := 1, 1
	for _, r := range source[:offset] {
		if r == '\n' {
			line, column = line+1, 1
		} else {
			column++
		}
	}
	return line, column
}

func (p *parser) emit(text string, position int) {
	p.output = append(p.output, text...)
	for range len(text) {
		p.positions = append(p.positions, position)
	}
}

func (p *parser) copy(start, end int) {
	p.output = append(p.output, p.source[start:end]...)
	for i := start; i < end; i++ {
		p.positions = append(p.positions, i)
	}
}

func (p *parser) space() error {
	for p.offset < len(p.source) {
		switch p.source[p.offset] {
		case ' ', '\t', '\r', '\n':
			p.offset++
		case '/':
			if !p.relaxed || p.offset+1 >= len(p.source) {
				return nil
			}
			switch p.source[p.offset+1] {
			case '/':
				p.offset += 2
				for p.offset < len(p.source) && p.source[p.offset] != '\n' {
					p.offset++
				}
			case '*':
				start := p.offset
				end := strings.Index(p.source[p.offset+2:], "*/")
				if end < 0 {
					return p.failure(start, "unterminated block comment")
				}
				p.offset += end + 4
			default:
				return nil
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *parser) value(depth int) error {
	if depth > MaxDepth {
		return p.failure(p.offset, "nesting exceeds the depth limit of 100")
	}
	if err := p.space(); err != nil {
		return err
	}
	if p.offset == len(p.source) {
		return p.failure(p.offset, "expected a JSON value")
	}
	switch p.source[p.offset] {
	case '{':
		return p.object(depth + 1)
	case '[':
		return p.array(depth + 1)
	case '"':
		return p.stringValue()
	case '\'':
		if p.relaxed {
			return p.stringValue()
		}
		return p.failure(p.offset, "single-quoted strings require JSON5-like mode")
	case 't':
		return p.literal("true")
	case 'f':
		return p.literal("false")
	case 'n':
		return p.literal("null")
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.number()
	case 'N', 'I':
		return p.failure(p.offset, "NaN and Infinity must be quoted strings under ProtoJSON rules")
	default:
		return p.failure(p.offset, "expected a JSON value")
	}
}

func (p *parser) object(depth int) error {
	if depth > MaxDepth {
		return p.failure(p.offset, "nesting exceeds the depth limit of 100")
	}
	p.emit("{", p.offset)
	p.offset++
	if err := p.space(); err != nil {
		return err
	}
	if p.consume('}') {
		return nil
	}
	for {
		if p.offset >= len(p.source) {
			return p.failure(p.offset, "expected an object key")
		}
		if p.source[p.offset] == '"' || p.relaxed && p.source[p.offset] == '\'' {
			if err := p.stringValue(); err != nil {
				return err
			}
		} else if p.relaxed && identifierStart(p.source[p.offset]) {
			start := p.offset
			for p.offset < len(p.source) && identifierPart(p.source[p.offset]) {
				p.offset++
			}
			p.emit("\"", start)
			p.copy(start, p.offset)
			p.emit("\"", p.offset-1)
		} else {
			return p.failure(p.offset, "expected a quoted object key")
		}
		if err := p.space(); err != nil {
			return err
		}
		if !p.consume(':') {
			return p.failure(p.offset, "expected ':' after object key")
		}
		if err := p.value(depth); err != nil {
			return err
		}
		if err := p.space(); err != nil {
			return err
		}
		if p.consume('}') {
			return nil
		}
		if p.offset >= len(p.source) || p.source[p.offset] != ',' {
			return p.failure(p.offset, "expected ',' or '}'")
		}
		comma := p.offset
		p.offset++
		if err := p.space(); err != nil {
			return err
		}
		if p.relaxed && p.consume('}') {
			return nil
		}
		p.emit(",", comma)
	}
}

func (p *parser) array(depth int) error {
	if depth > MaxDepth {
		return p.failure(p.offset, "nesting exceeds the depth limit of 100")
	}
	p.emit("[", p.offset)
	p.offset++
	if err := p.space(); err != nil {
		return err
	}
	if p.consume(']') {
		return nil
	}
	for {
		if err := p.value(depth); err != nil {
			return err
		}
		if err := p.space(); err != nil {
			return err
		}
		if p.consume(']') {
			return nil
		}
		if p.offset >= len(p.source) || p.source[p.offset] != ',' {
			return p.failure(p.offset, "expected ',' or ']'")
		}
		comma := p.offset
		p.offset++
		if err := p.space(); err != nil {
			return err
		}
		if p.relaxed && p.consume(']') {
			return nil
		}
		p.emit(",", comma)
	}
}

func (p *parser) consume(char byte) bool {
	if p.offset >= len(p.source) || p.source[p.offset] != char {
		return false
	}
	p.emit(string(char), p.offset)
	p.offset++
	return true
}

func (p *parser) stringValue() error {
	start := p.offset
	quote := p.source[p.offset]
	p.emit("\"", p.offset)
	p.offset++
	for p.offset < len(p.source) {
		position := p.offset
		char := p.source[p.offset]
		p.offset++
		if char == quote {
			p.emit("\"", position)
			return nil
		}
		if char < 0x20 {
			return p.failure(position, "unescaped control character in string")
		}
		if char == '\\' {
			if p.offset >= len(p.source) {
				return p.failure(position, "unterminated string escape")
			}
			escaped := p.source[p.offset]
			p.offset++
			if quote == '\'' && escaped == '\'' {
				p.emit("'", position)
				continue
			}
			if escaped == 'u' {
				if p.offset+4 > len(p.source) {
					return p.failure(position, "incomplete Unicode escape")
				}
				for i := 0; i < 4; i++ {
					if !hexDigit(p.source[p.offset+i]) {
						return p.failure(p.offset+i, "invalid Unicode escape")
					}
				}
				p.copy(position, p.offset+4)
				p.offset += 4
			} else if strings.ContainsRune("\"\\/bfnrt", rune(escaped)) {
				p.copy(position, p.offset)
			} else {
				return p.failure(position, "invalid string escape")
			}
		} else if char == '"' {
			p.emit("\\\"", position)
		} else {
			p.copy(position, p.offset)
		}
	}
	return p.failure(start, "unterminated string")
}

func (p *parser) literal(literal string) error {
	if !strings.HasPrefix(p.source[p.offset:], literal) {
		return p.failure(p.offset, "invalid JSON literal")
	}
	start := p.offset
	p.offset += len(literal)
	p.copy(start, p.offset)
	return nil
}

func (p *parser) number() error {
	start := p.offset
	if p.source[p.offset] == '-' {
		p.offset++
	}
	if p.offset == len(p.source) {
		return p.failure(start, "incomplete number")
	}
	if p.source[p.offset] == '0' {
		p.offset++
	} else {
		if p.source[p.offset] < '1' || p.source[p.offset] > '9' {
			return p.failure(start, "invalid number; NaN and Infinity must be quoted")
		}
		for p.offset < len(p.source) && digit(p.source[p.offset]) {
			p.offset++
		}
	}
	if p.offset < len(p.source) && p.source[p.offset] == '.' {
		p.offset++
		if p.offset == len(p.source) || !digit(p.source[p.offset]) {
			return p.failure(p.offset, "expected digits after decimal point")
		}
		for p.offset < len(p.source) && digit(p.source[p.offset]) {
			p.offset++
		}
	}
	if p.offset < len(p.source) && (p.source[p.offset] == 'e' || p.source[p.offset] == 'E') {
		p.offset++
		if p.offset < len(p.source) && (p.source[p.offset] == '+' || p.source[p.offset] == '-') {
			p.offset++
		}
		if p.offset == len(p.source) || !digit(p.source[p.offset]) {
			return p.failure(p.offset, "expected exponent digits")
		}
		for p.offset < len(p.source) && digit(p.source[p.offset]) {
			p.offset++
		}
	}
	if p.offset < len(p.source) && (identifierPart(p.source[p.offset]) || p.source[p.offset] == '.') {
		return p.failure(p.offset, "invalid number; use decimal JSON notation")
	}
	// json.Valid checks the numeric grammar without materializing a Go number.
	if !json.Valid([]byte(p.source[start:p.offset])) {
		return p.failure(start, "invalid JSON number")
	}
	p.copy(start, p.offset)
	return nil
}

func digit(char byte) bool { return char >= '0' && char <= '9' }
func hexDigit(char byte) bool {
	return digit(char) || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F'
}
func identifierStart(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char == '_' || char == '$'
}
func identifierPart(char byte) bool { return identifierStart(char) || digit(char) }
