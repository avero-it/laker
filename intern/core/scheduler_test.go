package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestTriggerNeverOverlaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	var runs atomic.Int32
	s := NewScheduler(ctx, time.UTC, zerolog.Nop(), []Job{{Name: "slow", Run: func(context.Context) {
		runs.Add(1)
		<-release
	}}})
	done := make(chan struct{})
	go func() { s.Serve(); close(done) }()

	if err := s.Trigger("slow"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !s.Running("slow") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := s.Trigger("slow"); !errors.Is(err, ErrRunning) {
		t.Fatalf("second trigger: %v, want ErrRunning", err)
	}
	if err := s.Trigger("nope"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("unknown job: %v", err)
	}
	if !s.Next("slow").IsZero() {
		t.Error("a manual job has no next run")
	}

	close(release)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return")
	}
	if runs.Load() != 1 {
		t.Errorf("runs = %d", runs.Load())
	}
}
