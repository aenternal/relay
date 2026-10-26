package storage

import (
	"strings"

	"relay/internal/domain"
)

func validateGRPCOptions(g domain.GRPCDefinition, check func(string, int) bool) error {
	// Drafts may have an incomplete host:port or variable references. URLs and
	// resolver expressions are never accepted; Dial validates the final endpoint.
	if strings.ContainsAny(g.Endpoint, "/@?#\r\n\t ") {
		return invalid("gRPC endpoint must be host:port without URL credentials or a path")
	}
	if !check(g.Authority, 4096) || !check(g.TLS.ServerName, 4096) || strings.ContainsAny(g.Authority, "\r\n/@?#\t ") || strings.ContainsAny(g.TLS.ServerName, "\r\n\x00") {
		return invalid("invalid gRPC authority or TLS server name")
	}
	for _, id := range []string{g.TLS.CAFileID, g.TLS.ClientCertFileID, g.TLS.ClientKeyFileID} {
		if !check(id, 256) {
			return invalid("invalid gRPC TLS file grant")
		}
	}
	for _, ms := range []int{g.TimeoutMS, g.ConnectTimeoutMS} {
		if ms < 0 || ms > 24*60*60*1000 {
			return invalid("gRPC timeout must be between 0 and 24 hours")
		}
	}
	for _, bytes := range []int{g.MaxSendBytes, g.MaxReceiveBytes} {
		if bytes < 0 || bytes > domain.DefaultGRPCMessageBytes {
			return invalid("gRPC message limit must be between 0 and 4 MiB")
		}
	}
	if g.Compression != "" && g.Compression != "none" && g.Compression != "gzip" {
		return invalid("unsupported gRPC compression")
	}
	if len(g.Metadata) > 1024 {
		return invalid("too many gRPC metadata fields")
	}
	for _, entry := range g.Metadata {
		if !check(entry.Name, 4096) || !check(entry.Value, 65536) {
			return invalid("gRPC metadata exceeds its limit")
		}
		name := strings.TrimSuffix(strings.ToLower(entry.Name), "-bin")
		if sensitiveName(name) && !safeHeaderTemplate(name, entry.Value) {
			return invalid("sensitive gRPC metadata must reference an environment variable")
		}
	}
	return nil
}
