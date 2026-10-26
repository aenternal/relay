package app

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"relay/internal/domain"
)

func TestShutdownReleasesRetainedCookiesSecretsAndReservations(t *testing.T) {
	s := &Service{}
	httpState := s.initHTTP()
	endpoint, _ := url.Parse("https://example.test")
	httpState.jar.SetCookies(endpoint, []*http.Cookie{{Name: "session", Value: "retained-cookie-canary"}})
	if len(httpState.jar.Cookies(endpoint)) != 1 {
		t.Fatal("fixture cookie was not retained")
	}
	httpState.files["grant"] = httpFileGrant{path: "/native-selected-file"}
	grpcState := s.initGRPC()
	grpcState.reflections["request"] = "schema"
	grpcState.records["grpc"] = &grpcRecord{result: domain.GRPCExecution{State: "completed", MessageJSON: `{"value":"preview"}`}, cancel: func() {}, reserve: 1024}
	grpcState.reserved = 1024
	wsState := s.initWebSocket()
	_, cancel := context.WithCancel(t.Context())
	wsState.records["socket"] = &webSocketRecord{
		result: domain.WebSocketExecution{State: "closed"}, cancel: cancel,
		secrets: []string{"retained-secret-canary"}, reserve: 2048,
		history: []domain.WebSocketEvent{{Type: "received", Text: "preview"}},
	}
	wsState.reserved = 2048
	if err := Shutdown(s); err != nil {
		t.Fatal(err)
	}
	if httpState.jar != nil || len(httpState.files) != 0 || len(httpState.records) != 0 || httpState.reserved != 0 {
		t.Fatal("HTTP retained a cookie jar, file grant, response, or reservation")
	}
	if len(grpcState.records) != 0 || len(grpcState.reflections) != 0 || grpcState.reserved != 0 {
		t.Fatal("gRPC retained previews, reflection cache, or reservations")
	}
	if len(wsState.records) != 0 || wsState.reserved != 0 {
		t.Fatal("WebSocket retained history, secret snapshots, or reservations")
	}
}
