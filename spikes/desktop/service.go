package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"relay/internal/session"
)

type Batch struct {
	RunID  string          `json:"runId"`
	Events []session.Event `json:"events"`
}

type JobStatus struct {
	RunID     string `json:"runId"`
	State     string `json:"state"`
	Delivered uint64 `json:"delivered"`
}

type job struct {
	status   JobStatus
	cancel   context.CancelFunc
	ack      chan uint64
	expected uint64
}

// SpikeService is a disposable shell probe, not the application API.
type SpikeService struct {
	ctx     context.Context
	emit    func(context.Context, Batch) error
	mu      sync.Mutex
	next    uint64
	current *job
}

func (s *SpikeService) Start(count int, delayMS int) (string, error) {
	if count < 1 || count > 100000 || delayMS < 0 || delayMS > 1000 {
		return "", errors.New("spike: count must be 1..100000 and delay 0..1000 ms")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && s.current.status.State == "running" {
		return "", errors.New("spike: cancel the active job before starting another")
	}
	if s.emit == nil || s.ctx == nil {
		return "", errors.New("spike: shell is not initialized")
	}
	if err := s.ctx.Err(); err != nil {
		return "", fmt.Errorf("spike: shell has stopped: %w", err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.next++
	j := &job{status: JobStatus{RunID: fmt.Sprint(s.next), State: "running"}, cancel: cancel, ack: make(chan uint64, 1)}
	s.current = j
	go s.run(ctx, j, count, time.Duration(delayMS)*time.Millisecond)
	return j.status.RunID, nil
}

func (s *SpikeService) run(ctx context.Context, j *job, count int, delay time.Duration) {
	defer j.cancel()
	input := make(chan session.Event, 128)
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		defer close(input)
		for i := range count {
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			select {
			case input <- session.Event{Sequence: uint64(i + 1), Timestamp: time.Now(), Kind: "progress", PayloadPreview: "Synthetic event"}:
			case <-ctx.Done():
				return
			}
		}
	}()
	err := session.Deliver(ctx, input, session.SinkFunc(func(ctx context.Context, events []session.Event) error {
		last := events[len(events)-1].Sequence
		s.mu.Lock()
		j.expected = last
		s.mu.Unlock()
		if err := s.emit(ctx, Batch{RunID: j.status.RunID, Events: events}); err != nil {
			return err
		}
		// At most one batch is in flight across the Go → webview boundary.
		ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		select {
		case <-ackCtx.Done():
			return ackCtx.Err()
		case <-j.ack:
			s.mu.Lock()
			j.status.Delivered = last
			j.expected = 0
			s.mu.Unlock()
			return nil
		}
	}), session.DefaultBatchConfig())
	j.cancel()
	<-produced
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil:
		j.status.State = "completed"
	case errors.Is(err, context.Canceled):
		j.status.State = "cancelled"
	default:
		j.status.State = "failed: event delivery was not acknowledged"
	}
}

func (s *SpikeService) Ack(runID string, sequence uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.current
	if j == nil || j.status.RunID != runID || j.expected == 0 || j.expected != sequence {
		return
	}
	select {
	case j.ack <- sequence:
		j.expected = 0
	default:
	}
}

func (s *SpikeService) Cancel(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && s.current.status.RunID == runID {
		s.current.cancel()
	}
}

func (s *SpikeService) Status() JobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return JobStatus{State: "idle"}
	}
	return s.current.status
}
