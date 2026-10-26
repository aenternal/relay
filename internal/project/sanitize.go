package project

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"relay/internal/domain"
)

var reference = regexp.MustCompile(`^\{\{[A-Za-z_][A-Za-z0-9_.-]*\}\}$`)

func sensitive(name string) bool {
	n := strings.NewReplacer("-", "", "_", "", " ", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(name)))
	n = strings.TrimSuffix(n, "bin")
	for _, exact := range []string{"authorization", "proxyauthorization", "cookie", "setcookie", "key", "auth", "code", "credential", "credentials", "passwd", "pwd", "privatekey"} {
		if n == exact {
			return true
		}
	}
	return strings.Contains(n, "apikey") || strings.Contains(n, "password") || strings.HasSuffix(n, "token") || strings.HasSuffix(n, "secret") || strings.HasSuffix(n, "signature") || strings.HasSuffix(n, "credential")
}

func safeCredential(name, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || reference.MatchString(value) {
		return true
	}
	if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "proxy-authorization") {
		for _, scheme := range []string{"Bearer ", "Basic "} {
			if len(value) > len(scheme) && strings.EqualFold(value[:len(scheme)], scheme) && reference.MatchString(value[len(scheme):]) {
				return true
			}
		}
	}
	return false
}

func sanitizeDocument(doc *Document, opts Options) ([]Warning, []string, error) {
	warnings := []Warning{}
	warn := func(code, message string) {
		for _, w := range warnings {
			if w.Code == code {
				return
			}
		}
		if len(warnings) < MaxWarnings {
			warnings = append(warnings, Warning{Code: code, Path: "project", Message: message})
		}
	}
	known := map[string]bool{}
	credentialBytes := 0
	credentialLimit := false
	remember := func(value string) {
		if value != "" && !reference.MatchString(value) && !known[value] {
			credentialBytes += len(value)
			if len(known) >= 1024 || credentialBytes > 1<<20 {
				credentialLimit = true
				return
			}
			known[value] = true
		}
	}
	var authCollect func(*domain.HTTPAuth)
	authCollect = func(auth *domain.HTTPAuth) {
		if auth == nil {
			return
		}
		for _, value := range []string{auth.Password, auth.Token, auth.KeyValue} {
			remember(value)
		}
		if auth.Type == "basic" {
			remember(auth.Username)
		}
	}
	rowsCollect := func(rows []domain.Header) {
		for _, row := range rows {
			if sensitive(row.Name) && !safeCredential(row.Name, row.Value) {
				remember(row.Value)
				if strings.EqualFold(row.Name, "authorization") || strings.EqualFold(row.Name, "proxy-authorization") {
					if _, value, ok := strings.Cut(row.Value, " "); ok {
						remember(value)
					}
				}
				if strings.EqualFold(row.Name, "cookie") || strings.EqualFold(row.Name, "set-cookie") {
					req := http.Request{Header: http.Header{"Cookie": []string{row.Value}}}
					for _, cookie := range req.Cookies() {
						remember(cookie.Value)
					}
				}
			}
		}
	}
	urlCollect := func(raw string) {
		if reference.MatchString(raw) {
			return
		}
		if info, _ := userinfo(raw); info != "" {
			for _, value := range strings.SplitN(info, ":", 2) {
				if decoded, err := url.PathUnescape(value); err == nil {
					remember(decoded)
				} else {
					remember(value)
				}
			}
		}
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		if u.User != nil {
			remember(u.User.Username())
			if password, ok := u.User.Password(); ok {
				remember(password)
			}
		}
		for key, values := range u.Query() {
			if sensitive(key) {
				for _, value := range values {
					remember(value)
				}
			}
		}
	}
	for _, env := range doc.Environments {
		for _, v := range env.Variables {
			remember(v.SecretRef)
			if v.Secret || sensitive(v.Name) {
				remember(v.Value)
			}
		}
	}
	for _, c := range doc.Collections {
		authCollect(c.Auth)
	}
	for _, f := range doc.Folders {
		authCollect(f.Auth)
	}
	for _, r := range doc.Requests {
		if h := r.HTTP; h != nil {
			authCollect(&h.Auth)
			rowsCollect(h.Headers)
			rowsCollect(h.Query)
			rowsCollect(h.Form)
			urlCollect(h.URL)
			urlCollect(h.Proxy.URL)
		}
		if g := r.GRPC; g != nil {
			rowsCollect(g.Metadata)
		}
		if w := r.WebSocket; w != nil {
			rowsCollect(w.Headers)
			rowsCollect(w.Query)
			urlCollect(w.URL)
			urlCollect(w.Proxy.URL)
		}
	}
	if credentialLimit {
		return nil, nil, errors.New("project exceeds the credential sanitization limit")
	}
	literals := make([]string, 0, len(known))
	for value := range known {
		literals = append(literals, value)
	}
	slices.SortFunc(literals, func(a, b string) int { return len(b) - len(a) })
	variants := map[string]bool{}
	short := []string{}
	for _, literal := range literals {
		if len(literal) < 4 {
			short = append(short, literal)
		}
		for _, encoded := range credentialVariants(literal) {
			variants[encoded] = true
		}
	}
	ordered := make([]string, 0, len(variants))
	for variant := range variants {
		ordered = append(ordered, variant)
	}
	slices.SortFunc(ordered, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, len(ordered)*2)
	for _, variant := range ordered {
		pairs = append(pairs, variant, "[REDACTED]")
	}
	replacer := strings.NewReplacer(pairs...)
	mask := func(value string) string {
		for _, literal := range short {
			if strings.Contains(value, literal) {
				warn("credential-removed", "Known credential values were removed from portable project fields.")
				return "[REDACTED]"
			}
		}
		cleaned := replacer.Replace(value)
		if cleaned != value {
			warn("credential-removed", "Known credential values were removed from portable project fields.")
		}
		return cleaned
	}
	credential := func(name, value string) string {
		if !safeCredential(name, value) {
			warn("credential-removed", "Credential literals were removed; configure secret environment variables after import.")
			return ""
		}
		return value
	}
	cleanAuth := func(auth *domain.HTTPAuth) {
		if auth == nil {
			return
		}
		auth.Password = credential("password", auth.Password)
		auth.Token = credential("token", auth.Token)
		auth.KeyValue = credential("api-key", auth.KeyValue)
		if auth.Type == "basic" {
			auth.Username = credential("username", auth.Username)
		} else {
			auth.Username = mask(auth.Username)
		}
		auth.KeyName = mask(auth.KeyName)
	}
	cleanRows := func(rows []domain.Header) {
		for i := range rows {
			row := &rows[i]
			if sensitive(row.Name) {
				row.Value = credential(row.Name, row.Value)
			}
			row.Name = mask(row.Name)
			row.Value = mask(row.Value)
		}
	}
	cleanURL := func(raw string) string {
		if reference.MatchString(raw) {
			return raw
		}
		// Keep variable braces intact while parsing and preserve repeated query
		// values. url.URL.String percent-escapes braces, so restore them afterward.
		if info, without := userinfo(raw); info != "" {
			raw = without
			warn("credential-removed", "URL credentials were removed; configure authentication after import.")
		}
		u, err := url.Parse(raw)
		if err != nil {
			return mask(raw)
		}
		if u.User != nil {
			u.User = nil
			warn("credential-removed", "URL credentials were removed; configure authentication after import.")
		}
		values := u.Query()
		changed := false
		for key, list := range values {
			if sensitive(key) {
				for i := range list {
					cleaned := credential(key, list[i])
					if cleaned != list[i] {
						list[i] = cleaned
						changed = true
					}
				}
				values[key] = list
			}
		}
		if changed {
			u.RawQuery = values.Encode()
		}
		return mask(strings.NewReplacer("%7B", "{", "%7D", "}").Replace(u.String()))
	}
	cleanTLS := func(tls *domain.HTTPTLS) {
		if tls.CAFileID != "" || tls.ClientCertFileID != "" || tls.ClientKeyFileID != "" {
			warn("file-grants-removed", "File grants and TLS file references are local and were removed.")
		}
		tls.CAFileID, tls.ClientCertFileID, tls.ClientKeyFileID = "", "", ""
		tls.ServerName = mask(tls.ServerName)
	}
	doc.Workspace.Name = mask(doc.Workspace.Name)
	for i := range doc.Collections {
		v := &doc.Collections[i]
		v.Name = mask(v.Name)
		cleanAuth(v.Auth)
	}
	for i := range doc.Folders {
		v := &doc.Folders[i]
		v.Name = mask(v.Name)
		cleanAuth(v.Auth)
	}
	for i := range doc.Environments {
		e := &doc.Environments[i]
		e.Name = mask(e.Name)
		for j := range e.Variables {
			v := &e.Variables[j]
			if v.Secret || sensitive(v.Name) {
				v.Secret = true
				if v.Value != "" || v.SecretRef != "" {
					warn("secrets-removed", "Secret values and keyring references are omitted; re-enter secrets after import.")
				}
				v.Value, v.SecretRef = "", ""
			} else {
				v.SecretRef = ""
				value := mask(v.Value)
				if value != v.Value {
					v.Value = ""
					v.Secret = true
				}
			}
			v.Name = mask(v.Name)
		}
	}
	for i := range doc.Requests {
		r := &doc.Requests[i]
		r.Name = mask(r.Name)
		if h := r.HTTP; h != nil {
			h.Method = mask(h.Method)
			h.URL = cleanURL(h.URL)
			cleanAuth(&h.Auth)
			cleanRows(h.Headers)
			cleanRows(h.Query)
			cleanRows(h.PathParams)
			cleanRows(h.Form)
			cleanTLS(&h.TLS)
			h.Proxy.URL = cleanURL(h.Proxy.URL)
			if h.FileID != "" {
				warn("file-grants-removed", "File grants and TLS file references are local and were removed.")
			}
			h.FileID = ""
			if !opts.IncludeBodies {
				if h.Body != "" || len(h.Form) > 0 || len(h.Multipart) > 0 {
					warn("bodies-omitted", "Request bodies are omitted unless explicitly included.")
				}
				h.Body = ""
				for j := range h.Form {
					h.Form[j].Value = ""
				}
				for j := range h.Multipart {
					h.Multipart[j].Value = ""
				}
			} else {
				h.Body = mask(h.Body)
			}
			for j := range h.Multipart {
				p := &h.Multipart[j]
				p.Name, p.Value, p.ContentType = mask(p.Name), mask(p.Value), mask(p.ContentType)
				if p.FileID != "" || p.FileName != "" {
					warn("file-grants-removed", "File grants and file names are local and were removed.")
				}
				p.FileID, p.FileName = "", ""
			}
			h.PersistBody = opts.IncludeBodies
		}
		if g := r.GRPC; g != nil {
			cleanRows(g.Metadata)
			cleanTLS(&g.TLS)
			g.Endpoint, g.Authority, g.Method = mask(g.Endpoint), mask(g.Authority), mask(g.Method)
			if !opts.IncludeBodies {
				if g.Body != "" {
					warn("bodies-omitted", "Request bodies are omitted unless explicitly included.")
				}
				g.Body = ""
			} else {
				g.Body = mask(g.Body)
			}
			g.PersistBody = opts.IncludeBodies
		}
		if w := r.WebSocket; w != nil {
			w.URL = cleanURL(w.URL)
			cleanRows(w.Headers)
			cleanRows(w.Query)
			cleanTLS(&w.TLS)
			w.Proxy.URL = cleanURL(w.Proxy.URL)
			for j := range w.Subprotocols {
				w.Subprotocols[j] = mask(w.Subprotocols[j])
			}
			if w.FileID != "" {
				warn("file-grants-removed", "File grants and TLS file references are local and were removed.")
			}
			w.FileID = ""
			if !opts.IncludeBodies {
				if w.Body != "" {
					warn("bodies-omitted", "Request bodies are omitted unless explicitly included.")
				}
				w.Body = ""
			} else {
				w.Body = mask(w.Body)
			}
			w.PersistBody = opts.IncludeBodies
		}
	}
	for i := range doc.Schemas {
		doc.Schemas[i].Metadata.Name = mask(doc.Schemas[i].Metadata.Name)
	}
	// Artifact scanning must use the same raw/encoded patterns as text masking:
	// descriptor comments and defaults can contain encoded credential copies.
	return warnings, ordered, nil
}

func credentialVariants(value string) []string {
	return []string{
		value, url.QueryEscape(value), url.PathEscape(value),
		base64.StdEncoding.EncodeToString([]byte(value)), base64.RawStdEncoding.EncodeToString([]byte(value)),
		base64.URLEncoding.EncodeToString([]byte(value)), base64.RawURLEncoding.EncodeToString([]byte(value)),
		hex.EncodeToString([]byte(value)), strings.ToUpper(hex.EncodeToString([]byte(value))),
	}
}

func userinfo(raw string) (string, string) {
	_, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "", raw
	}
	authority := rest
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return "", raw
	}
	prefix := len(raw) - len(rest)
	return authority[:at], raw[:prefix] + rest[at+1:]
}
