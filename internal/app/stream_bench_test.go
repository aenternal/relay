package app

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"relay/internal/domain"
	"relay/internal/protobuf/registry"
)

// BenchmarkWebSocketBurst10000 includes real preview encoding and the bounded
// queue/history append path. Its byte metric is logical payload, not wire bytes.
// B/op measures cumulative allocations for a whole burst, not retained memory.
func BenchmarkWebSocketBurst10000(b *testing.B) {
	for _, size := range []int{1024, 64 << 10} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			data := bytes.Repeat([]byte("x"), size)
			b.SetBytes(int64(10000 * size))
			b.ReportAllocs()
			var last *webSocketRecord
			b.ResetTimer()
			for b.Loop() {
				s := &Service{}
				r := &webSocketRecord{result: domain.WebSocketExecution{State: "open"}}
				for range 10000 {
					preview := webSocketPreview("received", "in", "text", data, nil)
					s.wsMu.Lock()
					s.appendWebSocketEvent(r, preview)
					s.wsMu.Unlock()
				}
				last = r
			}
			b.StopTimer()
			b.ReportMetric(float64(last.queueBytes+last.historyBytes), "retained-budget-B")
			b.ReportMetric(float64(len(last.queue)), "pending-events")
			b.ReportMetric(float64(len(last.history)), "history-events")
			b.ReportMetric(float64(last.result.DroppedEvents), "dropped-events")
			runtime.KeepAlive(last)
		})
	}
}

func BenchmarkWebSocketPreview64KiB(b *testing.B) {
	for _, sensitive := range []bool{false, true} {
		b.Run(fmt.Sprintf("secret-%t", sensitive), func(b *testing.B) {
			data := bytes.Repeat([]byte("plain text with preview-canary payload.\n"), 2000)[:64<<10]
			var secrets []string
			if sensitive {
				secrets = []string{"preview-canary"}
			}
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				preview := webSocketPreview("received", "in", "text", data, secrets)
				runtime.KeepAlive(preview)
			}
		})
	}
}

// The transport has already decoded the wire message when this preview path
// runs. Network I/O and protobuf wire decoding are deliberately not timed.
func BenchmarkGRPCStreamPreview64KiB(b *testing.B) {
	reg, err := registry.New([]protoreflect.FileDescriptor{wrapperspb.File_google_protobuf_wrappers_proto})
	if err != nil {
		b.Fatal(err)
	}
	descriptor := wrapperspb.String("").ProtoReflect().Descriptor()
	message := dynamicpb.NewMessage(descriptor)
	value := strings.Repeat("plain text with preview-canary payload.\n", 2000)[:64<<10]
	message.Set(descriptor.Fields().ByName("value"), protoreflect.ValueOfString(value))
	wire, err := proto.Marshal(message)
	if err != nil {
		b.Fatal(err)
	}
	prepared := &grpcPrepared{registry: reg}
	for _, sensitive := range []bool{false, true} {
		b.Run(fmt.Sprintf("secret-%t", sensitive), func(b *testing.B) {
			var secrets []string
			if sensitive {
				secrets = []string{"preview-canary"}
			}
			b.SetBytes(int64(len(value)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				preview := grpcStreamPreview(prepared, message, wire, "received", 1)
				preview = sanitizeGRPCStreamMessage(preview, secrets)
				runtime.KeepAlive(preview)
			}
		})
	}
}
