// Package session contains UI-independent delivery of bounded event batches.
package session

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Event contains only the bounded preview intended for a UI, never a full body.
type Event struct {
	Sequence       uint64    `json:"sequence"`
	Timestamp      time.Time `json:"timestamp"`
	Kind           string    `json:"kind"`
	PayloadPreview string    `json:"payloadPreview"`
	Truncated      bool      `json:"truncated"`
}

type BatchConfig struct {
	MaxEvents       int
	MaxPreviewBytes int
	FlushInterval   time.Duration
}

func DefaultBatchConfig() BatchConfig {
	return BatchConfig{MaxEvents: 64, MaxPreviewBytes: 4096, FlushInterval: 16 * time.Millisecond}
}

// Sink must honor ctx when delivery blocks. A batch belongs to the sink after
// Emit returns; the batcher does not reuse its backing array.
type Sink interface {
	Emit(ctx context.Context, batch []Event) error
}

type SinkFunc func(context.Context, []Event) error

func (f SinkFunc) Emit(ctx context.Context, batch []Event) error { return f(ctx, batch) }

// Deliver preserves order and applies backpressure rather than growing an
// unbounded queue. Callers must use a bounded input channel and honor ctx while
// producing. Closing input flushes the last batch; cancellation discards pending
// previews so a closing tab never waits for its UI.
func Deliver(ctx context.Context, input <-chan Event, sink Sink, cfg BatchConfig) error {
	if sink == nil || cfg.MaxEvents <= 0 || cfg.MaxEvents > 2000 || cfg.MaxPreviewBytes <= 0 || cfg.MaxPreviewBytes > 1<<20 || cfg.FlushInterval <= 0 {
		return errors.New("event batching: invalid sink, batch size, preview size or flush interval")
	}
	timer := time.NewTicker(cfg.FlushInterval)
	defer timer.Stop()
	batch := make([]Event, 0, cfg.MaxEvents)
	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		if err := sink.Emit(ctx, batch); err != nil {
			return err
		}
		batch = make([]Event, 0, cfg.MaxEvents)
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-input:
			if !ok {
				return flush()
			}
			if len(event.PayloadPreview) > cfg.MaxPreviewBytes {
				end := cfg.MaxPreviewBytes
				for end > 0 && !utf8.RuneStart(event.PayloadPreview[end]) {
					end--
				}
				event.PayloadPreview = strings.Clone(event.PayloadPreview[:end])
				event.Truncated = true
			}
			batch = append(batch, event)
			if len(batch) == cfg.MaxEvents {
				if err := flush(); err != nil {
					return err
				}
			}
		case <-timer.C:
			if err := flush(); err != nil {
				return err
			}
		}
	}
}
