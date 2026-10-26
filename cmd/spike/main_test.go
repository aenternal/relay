package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestProtoCommandDynamicLosslessWire(t *testing.T) {
	var output, warnings bytes.Buffer
	err := run(context.Background(), []string{"proto", "-I", "../../testdata/proto", "-proto", "relay/v1/echo.proto", "-method", "/relay.test.v1.EchoService/Unary", "-body", "-", "-json5"}, strings.NewReader("{text: 'hello', sequence: 9223372036854775807,}"), &output, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		JSON json.RawMessage `json:"protoJSON"`
		Wire string          `json:"protobufHex"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(result.JSON, []byte(`"9223372036854775807"`)) || !strings.HasPrefix(result.Wire, "0a0568656c6c6f10") {
		t.Fatalf("dynamic Protobuf lost precision or wire encoding: %s", output.String())
	}
}

func TestProtoMethodListingAndInvalidInput(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"proto", "-I", "../../testdata/proto", "-proto", "relay/v1/echo.proto"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/relay.test.v1.EchoService/Chat") {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{nil, {"unknown"}, {"http", "-timeout", "0s"}, {"proto", "-proto", "absent.proto"}} {
		if err := run(context.Background(), args, nil, &out, &out); err == nil {
			t.Fatalf("invalid input accepted: %v", args)
		}
	}
}
