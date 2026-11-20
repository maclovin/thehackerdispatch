package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestRunIfDue(t *testing.T) {
	pool := storetest.New(t)
	ctx := context.Background()
	var runs atomic.Int32
	job := Job{
		Name:     "test",
		Interval: func(context.Context) time.Duration { return time.Hour },
		Run:      func(context.Context) error { runs.Add(1); return nil },
	}

	for range 3 {
		if err := RunIfDue(ctx, pool, discard, job); err != nil {
			t.Fatal(err)
		}
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("ran %d times within one interval, want 1", n)
	}

	storetest.MustExec(t, pool, "UPDATE job_runs SET last_started_at = now() - interval '2 hours'")
	if err := RunIfDue(ctx, pool, discard, job); err != nil {
		t.Fatal(err)
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("ran %d times after the interval passed, want 2", n)
	}
}

func TestRunIfDueOneAtATime(t *testing.T) {
	pool := storetest.New(t)
	ctx := context.Background()
	var running, maxRunning, runs atomic.Int32
	job := Job{
		Name:     "slow",
		Interval: func(context.Context) time.Duration { return 0 },
		Run: func(context.Context) error {
			n := running.Add(1)
			if n > maxRunning.Load() {
				maxRunning.Store(n)
			}
			time.Sleep(100 * time.Millisecond)
			running.Add(-1)
			runs.Add(1)
			return nil
		},
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := RunIfDue(ctx, pool, discard, job); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxRunning.Load() != 1 {
		t.Errorf("%d runs overlapped, want 1 at a time", maxRunning.Load())
	}
}

func TestRunIfDueRecordsError(t *testing.T) {
	pool := storetest.New(t)
	ctx := context.Background()
	job := Job{
		Name:     "broken",
		Interval: func(context.Context) time.Duration { return time.Hour },
		Run:      func(context.Context) error { return errors.New("boom") },
	}
	if err := RunIfDue(ctx, pool, discard, job); err != nil {
		t.Fatal(err)
	}
	var lastErr string
	if err := pool.QueryRow(ctx, "SELECT last_error FROM job_runs WHERE name = 'broken'").Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if lastErr != "boom" {
		t.Errorf("last_error = %q", lastErr)
	}
}
