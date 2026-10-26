// Command spike exercises Relay's stage 0 core without a desktop runtime.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"

	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/jsoncodec"
	"relay/internal/protobuf/registry"
	"relay/internal/transport/grpcclient"
	"relay/internal/transport/httpclient"
	"relay/internal/transport/httpclient/http10"
	h3 "relay/internal/transport/httpclient/http3"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type values []string

func (v *values) String() string         { return fmt.Sprint([]string(*v)) }
func (v *values) Set(value string) error { *v = append(*v, value); return nil }

type schemaFlags struct {
	files, includes, bodies values
	method                  string
	json5                   bool
}

func (s *schemaFlags) bind(f *flag.FlagSet) {
	f.Var(&s.files, "proto", "root .proto file; repeatable")
	f.Var(&s.includes, "I", "include directory; repeatable")
	f.Var(&s.bodies, "body", "body file; '-' reads stdin once; repeatable for streams")
	f.StringVar(&s.method, "method", "", "descriptor method path /package.Service/Method")
	f.BoolVar(&s.json5, "json5", false, "allow comments, single quotes, bare keys and trailing commas")
}

func (s *schemaFlags) load(ctx context.Context) (*registry.Registry, protoreflect.MethodDescriptor, error) {
	reg, err := importer.Compile(ctx, s.files, s.includes)
	if err != nil {
		return nil, nil, err
	}
	if s.method == "" {
		return reg, nil, nil
	}
	method, err := reg.Method(s.method)
	return reg, method, err
}

func (s *schemaFlags) messages(reg *registry.Registry, method protoreflect.MethodDescriptor, stdin io.Reader) ([]*dynamicpb.Message, error) {
	if len(s.bodies) == 0 {
		return nil, errors.New("provide at least one -body file (or '-' for stdin)")
	}
	if len(s.bodies) > 1024 {
		return nil, errors.New("spike accepts at most 1024 messages")
	}
	format := jsoncodec.StrictJSON
	if s.json5 {
		format = jsoncodec.JSON5Like
	}
	result := make([]*dynamicpb.Message, 0, len(s.bodies))
	stdinUsed := false
	total := 0
	for i, name := range s.bodies {
		if name == "-" {
			if stdinUsed {
				return nil, errors.New("stdin can be used for only one body")
			}
			stdinUsed = true
		}
		data, err := readBody(name, stdin, jsoncodec.MaxInputBytes)
		if err != nil {
			return nil, fmt.Errorf("body %d: %w", i+1, err)
		}
		total += len(data)
		if total > 16<<20 {
			return nil, errors.New("spike body input exceeds 16 MiB total")
		}
		message, err := jsoncodec.Decode(reg, method.Input(), string(data), format)
		if err != nil {
			return nil, fmt.Errorf("body %d: %w", i+1, err)
		}
		result = append(result, message)
	}
	return result, nil
}

type tlsFlags struct {
	ca       string
	insecure bool
}

func (t *tlsFlags) bind(f *flag.FlagSet) {
	f.StringVar(&t.ca, "ca", "", "additional PEM CA certificate file")
	f.BoolVar(&t.insecure, "insecure", false, "explicitly disable certificate verification for this invocation")
}

func (t *tlsFlags) config(warnings io.Writer) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: t.insecure}
	if t.insecure {
		fmt.Fprintln(warnings, "WARNING: TLS certificate verification is disabled for this invocation")
	}
	if t.ca != "" {
		data, err := readBody(t.ca, nil, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("CA file contains no PEM certificates")
		}
		config.RootCAs = roots
	}
	return config, nil
}

func readBody(name string, stdin io.Reader, limit int64) ([]byte, error) {
	var r io.Reader = stdin
	if name != "-" {
		file, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		r = file
	}
	if r == nil {
		return nil, errors.New("input is not available")
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	return data, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: spike proto|grpc|http [flags]; see README.md")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(stderr)
	var schema schemaFlags
	var security tlsFlags
	timeout := f.Duration("timeout", 15*time.Second, "total timeout")
	switch args[0] {
	case "proto":
		schema.bind(f)
	case "grpc":
		schema.bind(f)
		security.bind(f)
	case "http":
		security.bind(f)
	default:
		return errors.New("unknown command; use proto, grpc or http")
	}
	endpoint := f.String("endpoint", "", "gRPC host:port")
	plaintext := f.Bool("plaintext", false, "gRPC: explicitly use unencrypted HTTP/2")
	version := f.String("version", "1.0", "HTTP version: 1.0 or 3 (no fallback)")
	url := f.String("url", "", "HTTP request URL")
	method := f.String("http-method", "GET", "HTTP method token")
	bodyPath := f.String("http-body", "", "HTTP body file or '-' for stdin")
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 || *timeout <= 0 {
		return errors.New("unexpected positional arguments or non-positive timeout")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if args[0] == "http" {
		config, err := security.config(stderr)
		if err != nil {
			return err
		}
		var body []byte
		if *bodyPath != "" {
			body, err = readBody(*bodyPath, stdin, 5<<20)
			if err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, *method, *url, bytes.NewReader(body))
		if err != nil {
			return errors.New("HTTP request has an invalid method or URL")
		}
		var client interface {
			Do(*http.Request) (*http.Response, error)
			Close() error
		}
		cfg := httpclient.Config{TLSConfig: config, Timeout: *timeout}
		switch *version {
		case "1.0":
			client, err = http10.New(cfg)
		case "3":
			client, err = h3.New(cfg)
		default:
			return errors.New("spike supports explicit HTTP versions 1.0 and 3")
		}
		if err != nil {
			return err
		}
		defer client.Close()
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		return enc.Encode(struct {
			Protocol string      `json:"protocol"`
			Status   int         `json:"status"`
			Headers  http.Header `json:"headers"`
			Trailers http.Header `json:"trailers"`
			Body     []byte      `json:"bodyBase64"`
		}{response.Proto, response.StatusCode, response.Header, response.Trailer, data})
	}
	reg, descriptor, err := schema.load(ctx)
	if err != nil {
		return err
	}
	if descriptor == nil {
		if args[0] == "grpc" {
			return errors.New("gRPC requires -method /package.Service/Method")
		}
		return enc.Encode(reg.Methods())
	}
	messages, err := schema.messages(reg, descriptor, stdin)
	if err != nil {
		return err
	}
	if args[0] == "proto" {
		for _, message := range messages {
			canonical, err := jsoncodec.Encode(reg, message)
			if err != nil {
				return err
			}
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
			if err != nil {
				return err
			}
			if err := enc.Encode(struct {
				JSON json.RawMessage `json:"protoJSON"`
				Wire string          `json:"protobufHex"`
			}{canonical, hex.EncodeToString(wire)}); err != nil {
				return err
			}
		}
		return nil
	}
	if !descriptor.IsStreamingClient() && len(messages) != 1 {
		return errors.New("selected method requires exactly one -body")
	}
	var config *tls.Config
	if *plaintext {
		if security.ca != "" || security.insecure {
			return errors.New("plaintext cannot be combined with TLS options")
		}
	} else {
		config, err = security.config(stderr)
		if err != nil {
			return err
		}
	}
	client, err := grpcclient.Dial(ctx, *endpoint, grpcclient.Options{Plaintext: *plaintext, TLSConfig: config})
	if err != nil {
		return err
	}
	defer client.Close()
	writeMessage := func(message proto.Message) error {
		body, err := jsoncodec.Encode(reg, message)
		if err != nil {
			return err
		}
		return enc.Encode(json.RawMessage(body))
	}
	if !descriptor.IsStreamingClient() && !descriptor.IsStreamingServer() {
		result, err := client.Unary(ctx, descriptor, messages[0], nil)
		if err != nil {
			return err
		}
		return writeMessage(result.Message)
	}
	stream, err := client.NewStream(ctx, descriptor, nil)
	if err != nil {
		return err
	}
	defer stream.Cancel()
	received := make(chan error, 1)
	go func() {
		for {
			message, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				received <- nil
				return
			}
			if err == nil {
				err = writeMessage(message)
			}
			if err != nil {
				stream.Cancel()
				received <- err
				return
			}
		}
	}()
	var sendErr error
	for _, message := range messages {
		if sendErr = stream.Send(message); sendErr != nil {
			break
		}
	}
	if sendErr == nil {
		sendErr = stream.CloseSend()
	}
	if sendErr != nil && !errors.Is(sendErr, io.EOF) {
		stream.Cancel()
	}
	recvErr := <-received
	// Cancel used to interrupt the receiver must not hide the original local
	// send failure. A non-cancellation peer status remains authoritative.
	if sendErr != nil && !errors.Is(sendErr, io.EOF) && status.Code(recvErr) == codes.Canceled {
		return sendErr
	}
	if recvErr != nil {
		return recvErr
	}
	if errors.Is(sendErr, io.EOF) {
		return nil
	}
	return sendErr
}
