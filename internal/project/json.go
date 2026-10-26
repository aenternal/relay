package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// GuardJSON runs before typed decoding to bound depth and allocation overhead.
// Duplicate object keys are rejected so preview and apply cannot disagree.
func GuardJSON(data []byte) error {
	if len(data) == 0 || len(data) > MaxBytes || !utf8.Valid(data) {
		return errors.New("project JSON must contain valid UTF-8 and be at most 32 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	remaining := 200000
	var scan func(int) error
	scan = func(depth int) error {
		remaining--
		if depth > 32 || remaining < 0 {
			return errors.New("project JSON exceeds its depth or token limit")
		}
		token, err := d.Token()
		if err != nil {
			return errors.New("invalid project JSON")
		}
		switch token {
		case json.Delim('{'):
			keys := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return errors.New("invalid project JSON")
				}
				remaining--
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("duplicate project JSON property")
				}
				keys[name] = true
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
			if token, err := d.Token(); err != nil || token != json.Delim('}') {
				return errors.New("invalid project JSON")
			}
		case json.Delim('['):
			for d.More() {
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
			if token, err := d.Token(); err != nil || token != json.Delim(']') {
				return errors.New("invalid project JSON")
			}
		case json.Delim('}'), json.Delim(']'):
			return errors.New("invalid project JSON")
		}
		return nil
	}
	if err := scan(0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("project JSON must contain one document")
	}
	return nil
}
