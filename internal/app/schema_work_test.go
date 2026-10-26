package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProtoWorkBudgetFailsFastAndFollowsApplicationShutdown(t *testing.T) {
	lifecycle, shutdown := context.WithCancel(t.Context())
	defer shutdown()
	first, releaseFirst, err := acquireProtoWork(t.Context(), lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	_, releaseSecond, err := acquireProtoWork(t.Context(), lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	if _, _, err := acquireProtoWork(t.Context(), lifecycle); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("full work budget did not reject another large operation: %v", err)
	}
	deadline, ok := first.Deadline()
	if !ok || time.Until(deadline) > protoWorkTimeout {
		t.Fatal("work has no bounded deadline")
	}
	shutdown()
	select {
	case <-first.Done():
	case <-time.After(time.Second):
		t.Fatal("application shutdown did not cancel active schema work")
	}
	if _, _, err := acquireProtoWork(t.Context(), lifecycle); !errors.Is(err, context.Canceled) {
		t.Fatalf("new work was accepted after application shutdown: %v", err)
	}
}
