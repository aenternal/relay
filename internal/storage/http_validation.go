package storage

import (
	"net/url"
	"relay/internal/domain"
	"strings"
)

func validateHTTPAuth(auth domain.HTTPAuth, check func(string, int) bool) error {
	switch auth.Type {
	case "", "none", "inherit", "basic", "bearer", "api-key":
	default:
		return invalid("unsupported HTTP auth type")
	}
	for _, v := range []string{auth.Type, auth.Username, auth.Password, auth.Token, auth.KeyName, auth.KeyValue, auth.In} {
		if !check(v, 65536) {
			return invalid("HTTP auth field too large")
		}
	}
	for _, v := range []string{auth.Password, auth.Token, auth.KeyValue} {
		if v != "" && !template.MatchString(v) {
			return invalid("HTTP credentials must reference secret environment variables")
		}
	}
	if auth.In != "" && auth.In != "header" && auth.In != "query" {
		return invalid("API key location must be header or query")
	}
	return nil
}

func validateHTTPOptions(h domain.HTTPDefinition, check func(string, int) bool) error {
	if err := validateHTTPAuth(h.Auth, check); err != nil {
		return err
	}
	switch h.BodyKind {
	case "", "none", "raw", "json", "json5", "form", "multipart", "file":
	default:
		return invalid("unsupported HTTP body kind")
	}
	switch h.Version {
	case "", "auto", "1.0", "1.1", "2", "h2c", "3":
	default:
		return invalid("unsupported HTTP version")
	}
	if len(h.Query) > 1024 || len(h.PathParams) > 1024 || len(h.Form) > 1024 || len(h.Multipart) > 1024 {
		return invalid("too many HTTP fields")
	}
	for _, rows := range [][]domain.Header{h.Query, h.PathParams, h.Form} {
		for _, field := range rows {
			if !check(field.Name, 4096) || !check(field.Value, maxBodyBytes) {
				return invalid("HTTP field too large")
			}
		}
	}
	for _, field := range h.Query {
		if sensitiveName(field.Name) && field.Value != "" && !template.MatchString(field.Value) {
			return invalid("sensitive query values must reference an environment variable")
		}
	}
	for _, field := range h.Multipart {
		for _, v := range []string{field.Name, field.Value, field.FileID, field.FileName, field.ContentType} {
			if !check(v, maxBodyBytes) {
				return invalid("multipart field too large")
			}
		}
	}
	for _, v := range []string{h.FileID, h.TLS.CAFileID, h.TLS.ClientCertFileID, h.TLS.ClientKeyFileID, h.TLS.ServerName} {
		if !check(v, 4096) {
			return invalid("HTTP file or TLS field too large")
		}
	}
	if !check(h.Proxy.URL, 65536) {
		return invalid("proxy URL too large")
	}
	switch h.Proxy.Mode {
	case "", "none", "environment", "custom":
	default:
		return invalid("unsupported proxy mode")
	}
	if h.Proxy.URL != "" {
		u, err := url.Parse(anyTemplate.ReplaceAllString(h.Proxy.URL, "variable"))
		if err != nil {
			return invalid("invalid proxy URL")
		}
		if u.User != nil {
			// Whole-URL variables avoid serializing URL userinfo. Individual
			// credentials can also be stored as variable references only.
			raw, _, _ := strings.Cut(h.Proxy.URL, "@")
			_, raw, _ = strings.Cut(raw, "://")
			username, password, hasPassword := strings.Cut(raw, ":")
			if !template.MatchString(username) || (hasPassword && !template.MatchString(password)) {
				return invalid("proxy credentials must reference environment variables")
			}
		}
	}
	for _, ms := range []int{h.TimeoutMS, h.ConnectTimeoutMS, h.TLSHandshakeTimeoutMS, h.ResponseHeaderTimeoutMS} {
		if ms < 0 || ms > 24*60*60*1000 {
			return invalid("HTTP timeout out of range")
		}
	}
	if h.MaxRedirects < 0 || h.MaxRedirects > 100 {
		return invalid("HTTP redirect limit out of range")
	}
	if h.MaxDownloadBytes < 0 || h.MaxDownloadBytes > 2<<30 {
		return invalid("HTTP download limit out of range")
	}
	return nil
}
