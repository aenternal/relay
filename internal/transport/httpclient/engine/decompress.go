package engine

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Preserve Content-Encoding for the UI; Uncompressed describes Body, and the
// original Content-Length remains visible in Header for transport inspection.
func decompressResponse(resp *http.Response) error {
	encoding := strings.Join(resp.Header.Values("Content-Encoding"), ",")
	if strings.TrimSpace(encoding) == "" {
		return nil
	}
	if strings.Count(encoding, ",") >= 8 {
		return errors.New("response Content-Encoding exceeds the limit of 8 compression layers")
	}
	encodings := strings.Split(encoding, ",")
	for _, value := range encodings {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "identity", "gzip", "x-gzip", "deflate":
		default:
			return errors.New("unsupported response Content-Encoding; disable decompression to inspect raw bytes")
		}
	}
	for i := len(encodings) - 1; i >= 0; i-- {
		value := strings.ToLower(strings.TrimSpace(encodings[i]))
		if value == "identity" {
			continue
		}
		resp.Body = &decoderBody{source: resp.Body, encoding: value}
		resp.Uncompressed = true
		resp.ContentLength = -1
	}
	return nil
}

// Decoder creation is lazy: returning response headers must not wait for an
// uploaded or downloaded body's first bytes. Limits belong to the consumer.
type decoderBody struct {
	source   io.ReadCloser
	encoding string
	once     sync.Once
	mu       sync.Mutex
	reader   io.ReadCloser
	err      error
}

func (b *decoderBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.once.Do(func() {
		switch b.encoding {
		case "gzip", "x-gzip":
			b.reader, b.err = gzip.NewReader(b.source)
		case "deflate":
			reader := bufio.NewReader(b.source)
			header, err := reader.Peek(2)
			if err != nil {
				b.err = err
				return
			}
			// RFC deflate uses a zlib wrapper. A few servers send raw DEFLATE;
			// recognize a valid zlib CMF/FLG pair without consuming input.
			if header[0]&15 == 8 && header[0]>>4 <= 7 && (int(header[0])*256+int(header[1]))%31 == 0 {
				b.reader, b.err = zlib.NewReader(reader)
			} else {
				b.reader = flate.NewReader(reader)
			}
		}
	})
	if b.err != nil {
		return 0, b.err
	}
	return b.reader.Read(p)
}

func (b *decoderBody) Close() error {
	// Close the underlying connection first to interrupt a concurrent blocked
	// Read. Decoder Close itself does not release the network response body.
	err := b.source.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reader != nil {
		_ = b.reader.Close()
	}
	return err
}
