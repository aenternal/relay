package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDeliverTenThousandInOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	input := make(chan Event, 128)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(input)
		for i := range 10000 {
			select {
			case input <- Event{Sequence: uint64(i + 1), PayloadPreview: strings.Repeat("я", 20)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	want := uint64(1)
	batches := 0
	err := Deliver(ctx, input, SinkFunc(func(_ context.Context, events []Event) error {
		batches++
		if len(events) > 64 {
			t.Fatalf("unbounded batch: %d", len(events))
		}
		for _, e := range events {
			if e.Sequence != want || len(e.PayloadPreview) > 7 || !utf8.ValidString(e.PayloadPreview) || !e.Truncated {
				t.Fatalf("invalid ordered, bounded event: %+v; want %d", e, want)
			}
			want++
		}
		return nil
	}), BatchConfig{64, 7, time.Hour})
	<-done
	if err != nil || want != 10001 || batches != 157 {
		t.Fatalf("err=%v next=%d batches=%d", err, want, batches)
	}
}

func TestDeliverTimerAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan Event, 1)
	input <- Event{Sequence: 1}
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Deliver(ctx, input, SinkFunc(func(ctx context.Context, events []Event) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}), BatchConfig{64, 4096, time.Millisecond})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("partial batch was not flushed")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked delivery did not stop")
	}
}

func TestDeliverSinkFailureAndInvalidConfig(t *testing.T) {
	input := make(chan Event, 1)
	input <- Event{}
	close(input)
	failure := errors.New("bridge closed")
	sink := SinkFunc(func(context.Context, []Event) error { return failure })
	if err := Deliver(context.Background(), input, sink, DefaultBatchConfig()); !errors.Is(err, failure) {
		t.Fatalf("sink error lost: %v", err)
	}
	if err := Deliver(context.Background(), input, sink, BatchConfig{}); err == nil {
		t.Fatal("invalid config accepted")
	}
}
