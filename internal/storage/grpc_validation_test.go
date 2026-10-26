package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"relay/internal/domain"
)

func grpcFixture() domain.State {
	state := fixture()
	state.Requests[0].HTTP = nil
	state.Requests[0].Protocol = "grpc"
	state.Requests[0].GRPC = &domain.GRPCDefinition{
		Endpoint: "[::1]:50051", Method: "/catalog.v1.Catalog/Get", BodyFormat: "json5",
		Plaintext: true, Authority: "catalog.test", Compression: "gzip",
		Metadata:  []domain.Header{{Name: "authorization", Value: "Bearer {{token}}", Enabled: true}, {Name: "x-debug-bin", Value: "AAEC", Enabled: true}},
		TLS:       domain.HTTPTLS{CAFileID: "transient-ca-grant", ClientCertFileID: "transient-cert-grant", ClientKeyFileID: "transient-key-grant", ServerName: "catalog.test"},
		TimeoutMS: 5000, ConnectTimeoutMS: 1000, MaxSendBytes: 1 << 20, MaxReceiveBytes: 2 << 20,
		Body: "transient-unary-body-canary-a8c1",
	}
	return state
}

func TestGRPCOptionsPersistAndTransientGrantsNeverReachSQLite(t *testing.T) {
	db, filename := openTestDB(t)
	source := grpcFixture()
	saved, err := db.Save(context.Background(), source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if source.Requests[0].GRPC.Body == "" || source.Requests[0].GRPC.TLS.CAFileID == "" {
		t.Fatal("Save modified caller-owned transient draft")
	}
	if g := saved.Requests[0].GRPC; g.Body != "" || g.TLS.CAFileID != "" || g.TLS.ClientCertFileID != "" || g.TLS.ClientKeyFileID != "" {
		t.Fatal("transient body or file grants retained")
	}
	saved.Requests[0].GRPC.Metadata[0].Value = "changed"
	if source.Requests[0].GRPC.Metadata[0].Value != "Bearer {{token}}" {
		t.Fatal("metadata aliases caller-owned draft")
	}
	for _, name := range []string{filename, filename + "-wal"} {
		data, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{source.Requests[0].GRPC.Body, "transient-ca-grant", "transient-cert-grant", "transient-key-grant"} {
			if bytes.Contains(data, []byte(canary)) {
				t.Fatal("transient request data persisted")
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := *source.Requests[0].GRPC
	want.Body = ""
	want.TLS.CAFileID, want.TLS.ClientCertFileID, want.TLS.ClientKeyFileID = "", "", ""
	if !reflect.DeepEqual(want, *loaded.Requests[0].GRPC) {
		t.Fatal("gRPC settings changed after restart")
	}
}

func TestGRPCRejectsSensitiveLiteralMetadataAndUnsafeSettings(t *testing.T) {
	canary := "private-grpc-metadata-canary-37ac"
	for _, name := range []string{"authorization", "authorization-bin", "x-api-key", "x-api-key-bin", "refresh_token", "client-secret", "cookie"} {
		t.Run(name, func(t *testing.T) {
			db, _ := openTestDB(t)
			state := grpcFixture()
			state.Requests[0].GRPC.Metadata = []domain.Header{{Name: name, Value: canary, Enabled: false}}
			_, err := db.Save(context.Background(), state, 0)
			if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), canary) {
				t.Fatalf("sensitive literal accepted or exposed: %v", err)
			}
		})
	}
	for name, modify := range map[string]func(*domain.GRPCDefinition){
		"URL credentials": func(g *domain.GRPCDefinition) { g.Endpoint = "https://name:" + canary + "@host:443" },
		"resolver URL":    func(g *domain.GRPCDefinition) { g.Endpoint = "dns:///host:443" },
		"timeout":         func(g *domain.GRPCDefinition) { g.TimeoutMS = -1 },
		"send limit":      func(g *domain.GRPCDefinition) { g.MaxSendBytes = 8 << 20 },
		"receive limit":   func(g *domain.GRPCDefinition) { g.MaxReceiveBytes = -1 },
		"authority":       func(g *domain.GRPCDefinition) { g.Authority = "host\r\nsecret" },
		"compression":     func(g *domain.GRPCDefinition) { g.Compression = "unsupported" },
	} {
		t.Run(name, func(t *testing.T) {
			db, _ := openTestDB(t)
			state := grpcFixture()
			modify(state.Requests[0].GRPC)
			if _, err := db.Save(context.Background(), state, 0); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), canary) {
				t.Fatalf("unsafe settings accepted or exposed: %v", err)
			}
		})
	}
}

func TestGRPCBodyPersistenceRequiresExplicitConsent(t *testing.T) {
	db, _ := openTestDB(t)
	state := grpcFixture()
	state.Requests[0].GRPC.PersistBody = true
	saved, err := db.Save(context.Background(), state, 0)
	if err != nil || saved.Requests[0].GRPC.Body != state.Requests[0].GRPC.Body {
		t.Fatalf("explicit body consent not honored: %v", err)
	}
}
