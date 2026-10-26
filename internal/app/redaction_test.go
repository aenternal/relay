package app

import (
	"strings"
	"testing"

	"relay/internal/security"
)

func TestSharedRedactionNeverReprocessesMarkersOrOverlappingSecrets(t *testing.T) {
	if got := redactHTTP("EEE", []string{"E", "E"}, 100); got != strings.Repeat(security.RedactedValue, 3) {
		t.Fatalf("redaction markers were processed as new input: %q", got)
	}
	if got := redactHTTP("zabc", []string{"bc", "abc"}, 100); got != "z"+security.RedactedValue {
		t.Fatalf("overlapping secret suffix was exposed: %q", got)
	}
	if got := redactHTTP("a b|a+b|a%20b|YSBi", []string{"a b"}, 100); got != strings.TrimSuffix(strings.Repeat(security.RedactedValue+"|", 4), "|") {
		t.Fatalf("encoded secret variant was not redacted: %q", got)
	}
}

func TestSharedRedactionBoundsExpansionBeforeAllocatingCompleteOutput(t *testing.T) {
	input := strings.Repeat("E", 4<<20)
	const limit = 4096
	got := redactHTTP(input, []string{"E", "E", "REDACTED"}, limit)
	if len(got) != limit || !strings.HasPrefix(got, strings.Repeat(security.RedactedValue, 10)) {
		t.Fatal("frequent secret replacement did not stop at output budget")
	}
}
