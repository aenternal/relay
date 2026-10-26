package app

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strings"

	"relay/internal/security"
)

func redactionSecretVariants(secrets []string) []string {
	unique := map[string]struct{}{}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, variant := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret), base64.StdEncoding.EncodeToString([]byte(secret)), base64.RawStdEncoding.EncodeToString([]byte(secret)), base64.URLEncoding.EncodeToString([]byte(secret)), base64.RawURLEncoding.EncodeToString([]byte(secret))} {
			unique[variant] = struct{}{}
		}
		encoded, _ := json.Marshal(secret)
		if len(encoded) > 2 {
			unique[string(encoded[1:len(encoded)-1])] = struct{}{}
		}
	}
	patterns := make([]string, 0, len(unique))
	for pattern := range unique {
		patterns = append(patterns, pattern)
	}
	sort.Slice(patterns, func(i, j int) bool {
		if len(patterns[i]) == len(patterns[j]) {
			return patterns[i] < patterns[j]
		}
		return len(patterns[i]) > len(patterns[j])
	})
	return patterns
}

// redactText replaces input once, so a secret matching part of [REDACTED] can
// never re-process a marker. Streaming writes stop at the output budget before
// short or frequently repeated secret values can amplify memory usage.
func redactText(value string, secrets []string, limit int) string {
	patterns := redactionSecretVariants(secrets)
	if len(patterns) == 0 {
		if limit > 0 && len(value) > limit {
			return value[:limit]
		}
		return value
	}
	pairs := make([]string, 0, len(patterns)*2)
	for _, pattern := range patterns {
		pairs = append(pairs, pattern, security.RedactedValue)
	}
	writer := &redactionWriter{remaining: limit, limited: limit > 0}
	_, _ = strings.NewReplacer(pairs...).WriteString(writer, value)
	return writer.text.String()
}

type redactionWriter struct {
	text      strings.Builder
	remaining int
	limited   bool
}

func (w *redactionWriter) Write(value []byte) (int, error) {
	return w.WriteString(string(value))
}

func (w *redactionWriter) WriteString(value string) (int, error) {
	if w.limited && len(value) > w.remaining {
		n, _ := w.text.WriteString(value[:w.remaining])
		w.remaining = 0
		return n, io.ErrShortWrite
	}
	n, err := w.text.WriteString(value)
	if w.limited {
		w.remaining -= n
	}
	return n, err
}
