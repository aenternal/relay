package storage

import (
	"net/url"
	"strings"

	"relay/internal/domain"
)

func validateWebSocketOptions(w domain.WebSocketDefinition, check func(string, int) bool) error {
	if !check(w.URL, 65536) || !check(w.Body, maxBodyBytes) || !check(w.FileID, 256) {
		return invalid("WebSocket definition exceeds its limit")
	}
	if err := validateURL(w.URL); err != nil {
		return err
	}
	u, err := url.Parse(anyTemplate.ReplaceAllString(w.URL, "variable"))
	if err != nil || u.Fragment != "" || u.Scheme != "" && u.Scheme != "ws" && u.Scheme != "wss" {
		return invalid("WebSocket URL must use ws or wss without a fragment")
	}
	if w.BodyKind != "" && w.BodyKind != "text" && w.BodyKind != "binary" {
		return invalid("unsupported WebSocket message kind")
	}
	if w.BodyEncoding != "" && w.BodyEncoding != "text" && w.BodyEncoding != "hex" && w.BodyEncoding != "base64" && w.BodyEncoding != "file" {
		return invalid("unsupported WebSocket message encoding")
	}
	if w.MaxMessageBytes < 0 || w.MaxMessageBytes > 4<<20 {
		return invalid("WebSocket message limit must be between 0 and 4 MiB")
	}
	if len(w.Headers) > 1024 || len(w.Query) > 1024 || len(w.Subprotocols) > 32 {
		return invalid("too many WebSocket handshake fields")
	}
	for _, row := range w.Headers {
		if !check(row.Name, 4096) || !check(row.Value, 65536) {
			return invalid("WebSocket header exceeds its limit")
		}
		if sensitiveName(row.Name) && !safeHeaderTemplate(row.Name, row.Value) {
			return invalid("sensitive WebSocket headers must reference an environment variable")
		}
	}
	for _, row := range w.Query {
		if !check(row.Name, 4096) || !check(row.Value, 65536) {
			return invalid("WebSocket query exceeds its limit")
		}
		if (sensitiveName(row.Name) || isWebSocketCredentialQuery(row.Name)) && row.Value != "" && !template.MatchString(strings.TrimSpace(row.Value)) {
			return invalid("sensitive WebSocket query must reference an environment variable")
		}
	}
	for _, protocol := range w.Subprotocols {
		if !check(protocol, 256) || strings.ContainsAny(protocol, "\r\n\x00") {
			return invalid("invalid WebSocket subprotocol")
		}
	}
	return validateHTTPOptions(domain.HTTPDefinition{TLS: w.TLS, Proxy: w.Proxy, ConnectTimeoutMS: w.ConnectTimeoutMS, TimeoutMS: w.WriteTimeoutMS, ResponseHeaderTimeoutMS: w.ReadTimeoutMS}, check)
}

func isWebSocketCredentialQuery(name string) bool {
	n := normalizeSensitiveName(name)
	return n == "key" || n == "auth" || n == "code"
}
