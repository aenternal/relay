package grpcclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc/credentials"
)

// Native verifiers can reject certificates without an x509 error subtype.
// Exercise the credential boundary with that error shape on every platform.
func TestObservedCredentialsCertificateDiagnosticSurvivesCancellation(t *testing.T) {
	const canary = "private-certificate-canary"
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "platform verification failure",
			err:  &tls.CertificateVerificationError{Err: errors.New("x509: " + canary)},
			want: "TLS certificate verification failed",
		},
		{
			name: "unknown authority remains specific",
			err:  &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			want: "TLS certificate is not signed by a trusted authority",
		},
		{
			name: "hostname remains specific",
			err:  &tls.CertificateVerificationError{Err: x509.HostnameError{Certificate: &x509.Certificate{}, Host: canary}},
			want: "TLS certificate does not match the server name",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := &connectionAttempts{}
			credential := &observedCredentials{
				TransportCredentials: failingHandshakeCredentials{TransportCredentials: credentials.NewTLS(&tls.Config{}), err: test.err},
				attempts:             attempts,
			}
			if _, _, err := credential.ClientHandshake(t.Context(), "localhost", nil); !errors.Is(err, test.err) {
				t.Fatalf("handshake did not preserve its verification error: %v", err)
			}
			if got := attempts.reason(); got != test.want || strings.Contains(got, canary) {
				t.Fatalf("verification diagnostic lost its category or exposed certificate text: %q", got)
			}
			// grpc-go cancels pending handshakes when Dial closes the connection.
			for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
				credential.TransportCredentials = failingHandshakeCredentials{TransportCredentials: credentials.NewTLS(&tls.Config{}), err: fmt.Errorf("handshake stopped: %w", cancellation)}
				_, _, _ = credential.ClientHandshake(t.Context(), "localhost", nil)
				if got := attempts.reason(); got != test.want || strings.Contains(got, canary) {
					t.Fatalf("verification diagnostic was lost or exposed certificate text: %q", got)
				}
			}
		})
	}
}

func TestConnectionCancellationIsNotCertificateRejection(t *testing.T) {
	attempts := &connectionAttempts{}
	for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
		attempts.record("TLS handshake failed", cancellation)
	}
	if got := attempts.reason(); strings.Contains(got, "TLS certificate") {
		t.Fatalf("cancellation was misclassified as a verification failure: %q", got)
	}
}

type failingHandshakeCredentials struct {
	credentials.TransportCredentials
	err error
}

func (c failingHandshakeCredentials) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, c.err
}
