package jsoncodec_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/jsoncodec"
)

func TestJSON5LikeLosslessDynamicRoundTrip(t *testing.T) {
	root, err := filepath.Abs("../../../testdata/proto")
	if err != nil {
		t.Fatal(err)
	}
	r, err := importer.Compile(context.Background(), []string{"relay/v1/echo.proto"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.Method("/relay.test.v1.EchoService/Unary")
	if err != nil {
		t.Fatal(err)
	}
	body := `{
  // Integers must never pass through float64.
  text: 'it\'s "valid"', sequence: 9223372036854775807,
  payload: {details: {label: 'nested', createdAt: '2026-10-23T12:34:56Z'}, counters: {max: '9223372036854775807'},},
  extra: {'@type': 'type.googleapis.com/relay.test.common.Details', label: 'dynamic Any'},
}`
	msg, err := jsoncodec.Decode(r, m.Input(), body, jsoncodec.JSON5Like)
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Get(m.Input().Fields().ByName("sequence")).Int(); got != math.MaxInt64 {
		t.Fatalf("integer rounded: %d", got)
	}
	wire, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) == 0 {
		t.Fatal("empty wire")
	}
	encoded, err := jsoncodec.Encode(r, msg)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jsoncodec.Decode(r, m.Input(), string(encoded), jsoncodec.StrictJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(msg, decoded) {
		t.Fatal("ProtoJSON round trip changed message")
	}
}

func TestRejectInvalidProtoJSON(t *testing.T) {
	root, _ := filepath.Abs("../../../testdata/proto")
	r, err := importer.Compile(context.Background(), []string{"relay/v1/echo.proto"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := r.Method("/relay.test.v1.EchoService/Unary")
	for _, tc := range []struct{ body, want string }{
		{`{unknown: 1}`, "unknown"},
		{`{sequence: 9223372036854775808}`, "sequence"},
		{`{choiceText: 'a', choiceNumber: 1}`, "oneof"},
		{`{text: 3}`, "text"},
		{`{text: 'first', text: 'second'}`, "duplicate"},
		{`{sequence: NaN}`, "quoted"},
		{`{sequence: 0x10}`, "number"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			_, err := jsoncodec.Decode(r, m.Input(), tc.body, jsoncodec.JSON5Like)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q: %v", tc.want, err)
			}
		})
	}
}

func TestNormalizePreservesTokensAndEscapes(t *testing.T) {
	for _, body := range []string{
		`{'text': 'it\'s "quoted"', unicode: '\u042f', slash: '\/', backslash: '\\'}`,
		`/*head*/{int64: 9223372036854775807, uint64: 18446744073709551615, exponent: -1.234e+30, arr: [true, null, false,],} // tail`,
		`{"text":"кириллица 🦜","literal":"/* keep // these */"}`,
	} {
		output, err := jsoncodec.Normalize(body, jsoncodec.JSON5Like)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(output) {
			t.Fatalf("normalization produced invalid JSON: %s", output)
		}
		if strings.Contains(body, "18446744073709551615") && !strings.Contains(string(output), "18446744073709551615") {
			t.Fatal("uint64 literal changed")
		}
	}
}

func TestProtoJSONDiagnosticUsesEditorPositions(t *testing.T) {
	root, _ := filepath.Abs("../../../testdata/proto")
	r, err := importer.Compile(context.Background(), []string{"relay/v1/echo.proto"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := r.Method("/relay.test.v1.EchoService/Unary")
	_, err = jsoncodec.Decode(r, m.Input(), "{\n  // Comments and UTF-8 alter normalized columns.\n  text: 'привет 🦜', sequence: 'wrong',\n}", jsoncodec.JSON5Like)
	if err == nil || !strings.Contains(err.Error(), "3:31") {
		t.Fatalf("expected editor position 3:31, got %v", err)
	}
}

func TestProtoJSONErrorsDoNotExposeInputValues(t *testing.T) {
	root, _ := filepath.Abs("../../../testdata/proto")
	r, err := importer.Compile(context.Background(), []string{"relay/v1/echo.proto"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := r.Method("/relay.test.v1.EchoService/Unary")
	const secret = "my-secret-token"
	for _, tc := range []struct{ body, reason string }{
		{`{sequence: 'my-secret-token'}`, "int64"},
		{`{payload: {counters: {private: 'my-secret-token'}}}`, "int64"},
		{`{choiceText: 'my-secret-token', choiceNumber: 1}`, "choiceNumber"},
		{`{text: 'my-secret-token', text: 'another value'}`, "text"},
		{`{extra: {'@type': 'https://user:my-secret-token@example.test/not.a.Message'}}`, "resolve"},
		{`{'my-secret-token': 'value'}`, "unknown field"},
		{`{payload: {counters: {'my-secret-token': 1, 'my-secret-token': 2}}}`, "duplicate map key"},
		{`{payload: {details: {createdAt: 'my-secret-token'}}}`, "Timestamp"},
		{`{extra: {'@type': 'type.googleapis.com/relay.test.common.Details', label: 'my-secret-token', createdAt: 'my-secret-token'}}`, "Timestamp"},
	} {
		_, err := jsoncodec.Decode(r, m.Input(), tc.body, jsoncodec.JSON5Like)
		if err == nil {
			t.Fatal("expected validation error")
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "https://user:") {
			t.Fatalf("secret exposed: %v", err)
		}
		if !strings.Contains(err.Error(), tc.reason) || !strings.Contains(err.Error(), " at 1:") {
			t.Fatalf("reason or position lost: %v", err)
		}
	}
	msg := dynamicpb.NewMessage(m.Input())
	extraField := m.Input().Fields().ByName("extra")
	extra := dynamicpb.NewMessage(extraField.Message())
	extra.Set(extraField.Message().Fields().ByName("type_url"), protoreflect.ValueOfString("https://user:"+secret+"@example.test/not.a.Message"))
	msg.Set(extraField, protoreflect.ValueOfMessage(extra))
	if _, err := jsoncodec.Encode(r, msg); err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("marshal error leaked value or lost reason: %v", err)
	}
}

func TestNormalizeSyntaxPositionsAndBounds(t *testing.T) {
	for _, body := range []string{"{\n key: [1,,2]}", "{'text': 'unterminated}", "{/* unterminated", `{"x": "\q"}`, `{"x": 01}`} {
		_, err := jsoncodec.Normalize(body, jsoncodec.JSON5Like)
		var syntax *jsoncodec.SyntaxError
		if !errors.As(err, &syntax) || syntax.Line < 1 || syntax.Column < 1 {
			t.Fatalf("missing position for %q: %v", body, err)
		}
	}
	for _, body := range []string{`{'text': 'a'}`, `{"text": "a",}`, "{// hi\n\"text\": \"a\"}"} {
		if _, err := jsoncodec.Normalize(body, jsoncodec.StrictJSON); err == nil {
			t.Fatalf("strict accepted %q", body)
		}
	}
	if _, err := jsoncodec.Normalize(strings.Repeat(" ", jsoncodec.MaxInputBytes+1), jsoncodec.JSON5Like); err == nil {
		t.Fatal("input limit not enforced")
	}
	if _, err := jsoncodec.Normalize(strings.Repeat("[", jsoncodec.MaxDepth+1)+strings.Repeat("]", jsoncodec.MaxDepth+1), jsoncodec.JSON5Like); err == nil {
		t.Fatal("depth limit not enforced")
	}
}
