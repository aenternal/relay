package app

import (
	"strings"
	"testing"
	"unicode/utf8"

	"relay/internal/security"
)

func TestWebSocketRedactionWithNewSecretsPreservesMarkersAndUTF8(t *testing.T) {
	first := webSocketText("E [REDACTED] later", []string{"E"}, 64<<10)
	second := webSocketText(first, []string{"E", "later"}, 64<<10)
	want := strings.Join([]string{security.RedactedValue, security.RedactedValue, security.RedactedValue}, " ")
	if second != want {
		t.Fatalf("new secret remasked an existing marker: %q", second)
	}
	preview := webSocketText(strings.Repeat("Я", 64<<10), nil, (64<<10)-1)
	if !utf8.ValidString(preview) || len(preview) > (64<<10)-1 || len(preview) < (64<<10)-2 {
		t.Fatalf("UTF-8 preview bound: bytes=%d valid=%v", len(preview), utf8.ValidString(preview))
	}
}
