package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Job struct {
	Name     string
	Interval func(context.Context) time.Duration
	Run      func(context.Context) error
}

const checkEvery = 30 * time.Second

func Loop(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, job Job) error {
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		if err := RunIfDue(ctx, pool, logger, job); err != nil && ctx.Err() == nil {
			logger.Error("job check failed", "job", job.Name, "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func RunSoon(ctx context.Context, pool *pgxpool.Pool, name string) error {
	_, err := pool.Exec(ctx, "UPDATE job_runs SET last_started_at = 'epoch' WHERE name = $1", name)
	return err
}

func RunIfDue(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, job Job) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	key := "job:" + job.Name
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", key).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer func() {
		// Unlock even if ctx was canceled; the lock belongs to this connection.
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtextextended($1, 0))", key); err != nil {
			logger.Error("release job lock", "job", job.Name, "err", err)
		}
	}()

	var last time.Time
	err = conn.QueryRow(ctx, "SELECT last_started_at FROM job_runs WHERE name = $1", job.Name).Scan(&last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && time.Since(last) < job.Interval(ctx) {
		return nil
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO job_runs (name, last_started_at) VALUES ($1, now())
		ON CONFLICT (name) DO UPDATE SET last_started_at = now()`, job.Name); err != nil {
		return err
	}

	start := time.Now()
	runErr := job.Run(ctx)
	errText := ""
	if runErr != nil {
		errText = runErr.Error()
		logger.Error("job failed", "job", job.Name, "err", runErr, "duration", time.Since(start))
	} else {
		logger.Info("job finished", "job", job.Name, "duration", time.Since(start))
	}
	_, err = conn.Exec(context.Background(),
		"UPDATE job_runs SET last_finished_at = now(), last_error = $2 WHERE name = $1", job.Name, errText)
	return err
}
