package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestWatcherStopBeforeStartAndCancelledStart(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			w, err := NewWatcher(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if cancelled {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if !errors.Is(w.Start(ctx), context.Canceled) {
					t.Fatal("cancelled watcher started")
				}
			}
			done := make(chan struct{})
			go func() { _ = w.Stop(); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Stop before Start deadlocked")
			}
			if w.Start(context.Background()) == nil {
				t.Fatal("stopped watcher restarted")
			}
		})
	}
}

func TestWatcherOverflowCannotBlockFlushOrShutdown(t *testing.T) {
	w, err := NewWatcher(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		w.record(fmt.Sprint(i), fsnotify.Write)
	}
	done := make(chan struct{})
	go func() { w.flush(); _ = w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("full event buffer blocked shutdown")
	}
	if err := <-w.Errors(); !errors.Is(err, ErrWatchEventOverflow) {
		t.Fatalf("overflow was not signalled: %v", err)
	}
}

func TestWatcherContinuousWritesHaveBoundedDebounce(t *testing.T) {
	w, err := NewWatcher(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	w.debounceWindow = 30 * time.Millisecond
	w.record("state.db-wal", fsnotify.Write)
	timer := w.timer
	stopWrites := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWrites:
				return
			case <-ticker.C:
				w.record("state.db-wal", fsnotify.Write)
			}
		}
	}()
	defer func() { close(stopWrites); <-writerDone }()
	select {
	case <-timer.C:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("ongoing writes kept extending the debounce window")
	}
}
