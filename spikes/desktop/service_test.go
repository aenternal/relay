package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func awaitStatus(t *testing.T, s *SpikeService, want string) JobStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state := s.Status()
		if state.State == want {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job did not reach %q: %+v", want, s.Status())
	return JobStatus{}
}

func TestJobBatchesAcknowledgementsAndRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &SpikeService{ctx: ctx}
	var received atomic.Uint64
	s.emit = func(_ context.Context, batch Batch) error {
		for _, event := range batch.Events {
			if want := received.Add(1); event.Sequence != want {
				t.Errorf("sequence = %d; want %d", event.Sequence, want)
			}
		}
		last := batch.Events[len(batch.Events)-1].Sequence
		s.Ack("stale-run", last)
		s.Ack(batch.RunID, last-1)
		s.Ack(batch.RunID, last)
		s.Ack(batch.RunID, last) // Duplicate acknowledgement must not acknowledge the next batch.
		return nil
	}
	if _, err := s.Start(10000, 0); err != nil {
		t.Fatal(err)
	}
	state := awaitStatus(t, s, "completed")
	if state.Delivered != 10000 || received.Load() != 10000 {
		t.Fatalf("events lost: %+v; received %d", state, received.Load())
	}
	received.Store(0)
	if _, err := s.Start(1, 0); err != nil {
		t.Fatal(err)
	}
	if state := awaitStatus(t, s, "completed"); state.Delivered != 1 {
		t.Fatal(state)
	}
}

func TestJobCancellationUnblocksPendingAckAndProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 1)
	s := &SpikeService{ctx: ctx, emit: func(context.Context, Batch) error {
		entered <- struct{}{}
		return nil // Simulate a webview that never acknowledges.
	}}
	id, err := s.Start(100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no batch")
	}
	if _, err := s.Start(1, 0); err == nil {
		t.Fatal("concurrent job allowed")
	}
	s.Cancel("stale-run")
	if state := s.Status(); state.State != "running" {
		t.Fatal(state)
	}
	s.Cancel(id)
	awaitStatus(t, s, "cancelled")
}

func TestJobShutdownAndValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &SpikeService{ctx: ctx, emit: func(context.Context, Batch) error { return nil }}
	if _, err := s.Start(100001, 0); err == nil {
		t.Fatal("unbounded job allowed")
	}
	if _, err := s.Start(10000, 100); err != nil {
		t.Fatal(err)
	}
	cancel()
	awaitStatus(t, s, "cancelled")
}
