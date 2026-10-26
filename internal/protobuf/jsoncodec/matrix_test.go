package jsoncodec_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/jsoncodec"
	"relay/internal/protobuf/registry"
)

func matrix(t *testing.T) (*registry.Registry, protoreflect.MessageDescriptor) {
	t.Helper()
	root, _ := filepath.Abs("../../../testdata/proto")
	r, err := importer.Compile(context.Background(), []string{"matrix/v1/types.proto", "matrix/v2/required.proto"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.Message("relay.test.matrix.EveryType")
	if err != nil {
		t.Fatal(err)
	}
	return r, m
}

func TestCompleteScalarAndCollectionMatrix(t *testing.T) {
	r, m := matrix(t)
	body := `{
int32Value: -2147483648, sint32Value: 2147483647, sfixed32Value: -2147483648,
uint32Value: 4294967295, fixed32Value: 4294967295,
int64Value: -9223372036854775808, sint64Value: 9223372036854775807, sfixed64Value: -9223372036854775808,
uint64Value: 18446744073709551615, fixed64Value: '18446744073709551615',
floatValue: 'Infinity', doubleValue: '-Infinity', boolValue: true,
string_value: 'UTF-8 🦜', bytesValue: 'AP_-', status: 'READY', optionalCount: 0,
sequences: [-9223372036854775808, '9223372036854775807'],
indexes: {'-2147483648': 'min', '2147483647': 'max'}, toggles: {true: '18446744073709551615'},
children: {first: {label:'nested'}}, items:[{label:'one'},{label:'two'}], selectedNumber: 0,
}`
	message, err := jsoncodec.Decode(r, m, body, jsoncodec.JSON5Like)
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Get(m.Fields().ByName("uint64_value")).Uint(); got != math.MaxUint64 {
		t.Fatal("uint64 rounded:", got)
	}
	if got := message.Get(m.Fields().ByName("int64_value")).Int(); got != math.MinInt64 {
		t.Fatal("int64 rounded:", got)
	}
	if !message.Has(m.Fields().ByName("optional_count")) || !message.Has(m.Fields().ByName("selected_number")) {
		t.Fatal("explicit zero presence lost")
	}
	canonical, err := jsoncodec.Encode(r, message)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"18446744073709551615"`, `"-9223372036854775808"`, `"bytesValue": "AP/+"`, `"status": "READY"`} {
		if !bytes.Contains(canonical, []byte(expected)) {
			t.Fatalf("canonical lacks %s: %s", expected, canonical)
		}
	}
	round, err := jsoncodec.Decode(r, m, string(canonical), jsoncodec.StrictJSON)
	if err != nil || !proto.Equal(message, round) {
		t.Fatalf("JSON round trip: %v", err)
	}
	first, err := jsoncodec.EncodeBinary(message)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := jsoncodec.EncodeBinary(round)
	if !bytes.Equal(first, second) {
		t.Fatal("wire is not deterministic")
	}
	wireRound, err := jsoncodec.DecodeBinary(r, m, first)
	if err != nil || !proto.Equal(message, wireRound) {
		t.Fatalf("binary round trip: %v", err)
	}
}

func TestWKTAndAnyMatrix(t *testing.T) {
	r, m := matrix(t)
	body := `{
timestamp:'2026-10-24T01:02:03.123456789+03:00', duration:'-1.000000001s', mask:'child.label,stringValue',
object:{nested:{a:[true,null,'x',1.25]}}, value:null, list:[null,true,'x',{a:1}],
any:{'@type':'type.googleapis.com/relay.test.matrix.Child',label:'dynamic',next:{label:'nested'}},
empty:{}, wrappedInt64:'9223372036854775807',wrappedUint64:'18446744073709551615',
wrappedString:'wrapped',wrappedBytes:'_-4',wrappedBool:false,wrappedDouble:'NaN',wrappedFloat:'-Infinity',wrappedInt32:-2147483648,wrappedUint32:4294967295,
}`
	message, err := jsoncodec.Decode(r, m, body, jsoncodec.JSON5Like)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncodec.Encode(r, message)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"2026-10-23T22:02:03.123456789Z", "-1.000000001s", `"wrappedBytes": "/+4="`, `"NaN"`} {
		if !bytes.Contains(canonical, []byte(expected)) {
			t.Fatalf("canonical lacks %s: %s", expected, canonical)
		}
	}
	round, err := jsoncodec.Decode(r, m, string(canonical), jsoncodec.StrictJSON)
	if err != nil || !proto.Equal(message, round) {
		t.Fatalf("WKT JSON round trip: %v", err)
	}
	for _, any := range []string{
		`{"any":{"@type":"type.googleapis.com/google.protobuf.Timestamp","value":"2000-01-01T00:00:00Z"}}`,
		`{"any":{"@type":"type.googleapis.com/google.protobuf.Int64Value","value":"9223372036854775807"}}`,
		`{"any":{"@type":"type.googleapis.com/google.protobuf.Struct","value":{"n":1}}}`,
	} {
		message, err := jsoncodec.Decode(r, m, any, jsoncodec.StrictJSON)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := jsoncodec.Encode(r, message)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := jsoncodec.Decode(r, m, string(canonical), jsoncodec.StrictJSON); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScalarCollectionWKTFailuresHaveSafePaths(t *testing.T) {
	r, m := matrix(t)
	const secret = "secret-canary-should-never-appear"
	for _, tc := range []struct{ body, path string }{
		{`{int32Value:2147483648}`, "$.int32Value"},
		{`{uint32Value:-1}`, "$.uint32Value"},
		{`{int64Value:9223372036854775808}`, "$.int64Value"},
		{`{uint64Value:18446744073709551616}`, "$.uint64Value"},
		{`{floatValue:3.5e38}`, "$.floatValue"},
		{`{boolValue:'secret-canary-should-never-appear'}`, "$.boolValue"},
		{`{bytesValue:'secret-canary-should-never-appear!'}`, "$.bytesValue"},
		{`{status:'secret-canary-should-never-appear'}`, "$.status"},
		{`{sequences:[1,'secret-canary-should-never-appear']}`, "$.sequences[1]"},
		{`{indexes:{'secret-canary-should-never-appear':'v'}}`, "$.indexes[*]"},
		{`{toggles:{true:'secret-canary-should-never-appear'}}`, "$.toggles[*]"},
		{`{children:{'secret-canary-should-never-appear':{label:3}}}`, "$.children[*].label"},
		{`{items:[{label:'ok'},{label:3}]}`, "$.items[1].label"},
		{`{selectedText:'secret-canary-should-never-appear',selectedNumber:1}`, "$.selectedNumber"},
		{`{child:{'secret-canary-should-never-appear':1}}`, "$.child"},
		{`{timestamp:'secret-canary-should-never-appear'}`, "$.timestamp"},
		{`{duration:'secret-canary-should-never-appear'}`, "$.duration"},
		{`{mask:'secret-canary-should-never-appear_'}`, "$.mask"},
		{`{any:{'@type':'https://user:secret-canary-should-never-appear@example.test/missing.Message'}}`, "$.any"},
	} {
		t.Run(tc.path+tc.body[:8], func(t *testing.T) {
			_, err := jsoncodec.Decode(r, m, tc.body, jsoncodec.JSON5Like)
			var diagnostic *jsoncodec.ValidationError
			if !errors.As(err, &diagnostic) {
				t.Fatalf("expected structured diagnostic: %v", err)
			}
			if diagnostic.Path != tc.path {
				t.Fatalf("path: got %q want %q (%v)", diagnostic.Path, tc.path, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(diagnostic.Path, secret) {
				t.Fatalf("secret exposed: %v", err)
			}
			if diagnostic.Line != 1 || diagnostic.Column < 1 {
				t.Fatalf("missing editor coordinates: %+v", diagnostic)
			}
		})
	}
}

func TestProto2RequiredExtensionsAndSafeTemplate(t *testing.T) {
	r, _ := matrix(t)
	m, err := r.Message("relay.test.matrix2.RequiredNode")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jsoncodec.Decode(r, m, `{}`, jsoncodec.StrictJSON); err == nil || !strings.Contains(err.Error(), "required_text") {
		t.Fatalf("required missing: %v", err)
	}
	message, err := jsoncodec.Decode(r, m, `{"requiredText":"ok","[relay.test.matrix2.label]":"extension"}`, jsoncodec.StrictJSON)
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Get(m.Fields().ByName("default_number")).Int(); got != 17 {
		t.Fatalf("proto2 default missing: %d", got)
	}
	wire, err := jsoncodec.EncodeBinary(message)
	if err != nil {
		t.Fatal(err)
	}
	round, err := jsoncodec.DecodeBinary(r, m, wire)
	if err != nil || !proto.Equal(message, round) {
		t.Fatalf("extension wire: %v", err)
	}
	canonical, err := jsoncodec.Encode(r, round)
	if err != nil || !bytes.Contains(canonical, []byte("[relay.test.matrix2.label]")) {
		t.Fatalf("extension JSON: %s %v", canonical, err)
	}
	template, err := jsoncodec.Template(r, m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(template, []byte(`"requiredText": ""`)) || !bytes.Contains(template, []byte(`"choice": "FIRST"`)) || bytes.Contains(template, []byte(": 17")) || bytes.Contains(template, []byte("SECOND")) {
		t.Fatalf("unsafe source defaults: %s", template)
	}
	if _, err := jsoncodec.Decode(r, m, string(template), jsoncodec.StrictJSON); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateInspectionAndFiniteBounds(t *testing.T) {
	r, m := matrix(t)
	template, err := jsoncodec.Template(r, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jsoncodec.Decode(r, m, string(template), jsoncodec.StrictJSON); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(template, []byte("selectedNumber")) || !bytes.Contains(template, []byte("selectedText")) {
		t.Fatalf("oneof template: %s", template)
	}
	if bytes.Contains(template, []byte("\"next\"")) {
		t.Fatal("recursive edge included")
	}
	foundNested, foundPresence, foundMap, foundComment := false, false, false, false
	for _, message := range r.Messages() {
		if message.Name == "relay.test.matrix.EveryType.Nested" {
			foundNested = true
		}
		for _, field := range message.Fields {
			if field.Name == "optional_count" && field.Presence && field.Oneof == "" {
				foundPresence = true
			}
			if field.Name == "toggles" && field.Map && field.MapKey == "bool" && field.MapValue == "uint64" {
				foundMap = true
			}
			if field.Name == "string_value" && strings.Contains(field.Comment, "retained") {
				foundComment = true
			}
		}
	}
	if !foundNested || !foundPresence || !foundMap || !foundComment {
		t.Fatalf("inspection incomplete: %v %v %v %v", foundNested, foundPresence, foundMap, foundComment)
	}
	for _, info := range r.Messages() {
		if strings.HasSuffix(info.Name, "Entry") {
			t.Fatal("synthetic map entry exposed")
		}
	}
	impossible, err := importer.CompileSources(context.Background(), map[string]string{"required.proto": `syntax="proto2"; message Impossible { required Impossible child=1; }`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	desc, _ := impossible.Message("Impossible")
	if _, err := jsoncodec.Template(impossible, desc); err == nil {
		t.Fatal("required recursive template accepted")
	}
	for _, message := range r.Messages() {
		if strings.HasPrefix(message.Name, "google.protobuf.") {
			descriptor, _ := r.Message(message.Name)
			if _, err := jsoncodec.Template(r, descriptor); err != nil {
				t.Fatalf("WKT template %s: %v", message.Name, err)
			}
		}
	}
}

func TestBinaryMalformedUnknownFieldsAndBoundedPretty(t *testing.T) {
	r, m := matrix(t)
	for _, wire := range [][]byte{{0xff}, {0x72, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}} {
		if _, err := jsoncodec.DecodeBinary(r, m, wire); err == nil {
			t.Fatal("invalid wire accepted")
		}
	}
	if _, err := jsoncodec.DecodeBinary(r, m, make([]byte, jsoncodec.MaxInputBytes+1)); err == nil {
		t.Fatal("wire input bound missing")
	}
	// Field 100 is unknown and must survive through protobuf wire decoding.
	wire := []byte{0xa0, 0x06, 0x7b}
	message, err := jsoncodec.DecodeBinary(r, m, wire)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jsoncodec.EncodeBinary(message)
	if err != nil || !bytes.Equal(out, wire) {
		t.Fatalf("unknown wire fields changed: %x %v", out, err)
	}
	// Dense data at depth 90 would allocate hundreds of MiB if blindly indented.
	body := `{"object":` + strings.Repeat(`{"n":`, 90) + `[` + strings.Repeat(`0,`, 100000) + `0]` + strings.Repeat(`}`, 90) + `}`
	message, err = jsoncodec.Decode(r, m, body, jsoncodec.StrictJSON)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := jsoncodec.Encode(r, message)
	if err != nil || len(preview) > jsoncodec.MaxOutputBytes {
		t.Fatalf("bounded encoding failed: %d %v", len(preview), err)
	}
}
