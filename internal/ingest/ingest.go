package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/maclovin/thehackerdispatch/internal/fetch"
)

const JobName = "ingest"

const (
	maxParallel         = 16
	maxParallelPerHost  = 2
	maxFailuresInARow   = 10
	maxStoredStatusText = 300
	firstBackoff        = 15 * time.Minute
	maxBackoff          = 24 * time.Hour
)

type Ingester struct {
	pool   *pgxpool.Pool
	client *fetch.Client
	logger *slog.Logger
}

func New(pool *pgxpool.Pool, client *fetch.Client, logger *slog.Logger) *Ingester {
	return &Ingester{pool: pool, client: client, logger: logger}
}

type Stats struct {
	Sources  int
	Failed   int
	NewItems int
}

type sourceState struct {
	ID           int64
	URL          string
	UserAgent    string
	ETag         string
	LastModified string
	CursorAt     *time.Time
	LastGUIDs    []string
	LastFetchAt  *time.Time
	Failures     int
}

func (in *Ingester) RunOnce(ctx context.Context) (Stats, error) {
	rows, err := in.pool.Query(ctx, `
		SELECT id, url, user_agent, etag, last_modified, cursor_at, last_guids, last_fetch_at, consecutive_failures
		FROM sources WHERE enabled AND (retry_after IS NULL OR retry_after <= now()) ORDER BY id`)
	if err != nil {
		return Stats{}, fmt.Errorf("list sources: %w", err)
	}
	states, err := pgx.CollectRows(rows, pgx.RowToStructByPos[sourceState])
	if err != nil {
		return Stats{}, fmt.Errorf("list sources: %w", err)
	}

	var (
		mu    sync.Mutex
		stats = Stats{Sources: len(states)}
		hosts = map[string]chan struct{}{}
	)
	for _, st := range states {
		host := hostOf(st.URL)
		if hosts[host] == nil {
			hosts[host] = make(chan struct{}, maxParallelPerHost)
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxParallel)
	for _, st := range states {
		g.Go(func() error {
			slot := hosts[hostOf(st.URL)]
			slot <- struct{}{}
			defer func() { <-slot }()

			n, err := in.fetchSource(gctx, st)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				stats.Failed++
				in.logger.Warn("source fetch failed", "source", st.ID, "url", st.URL, "err", err)
			}
			stats.NewItems += n
			return nil // one bad source must not stop the others
		})
	}
	_ = g.Wait()
	return stats, ctx.Err()
}

func (in *Ingester) fetchSource(ctx context.Context, st sourceState) (int, error) {
	header := http.Header{}
	if st.ETag != "" {
		header.Set("If-None-Match", st.ETag)
	}
	if st.LastModified != "" {
		header.Set("If-Modified-Since", st.LastModified)
	}
	if st.UserAgent != "" {
		header.Set("User-Agent", st.UserAgent)
	}

	resp, err := in.client.Get(ctx, st.URL, header)
	if err != nil {
		return 0, in.recordFailure(ctx, st.ID, err.Error(), 0)
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		_, err := in.pool.Exec(ctx, `
			UPDATE sources SET last_fetch_at = now(), last_status = 'not modified', consecutive_failures = 0, retry_after = NULL
			WHERE id = $1`, st.ID)
		return 0, err
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		wait := backoff(resp.Header, st.Failures, time.Now())
		return 0, in.recordFailure(ctx, st.ID, fmt.Sprintf("HTTP %d, next try in %s", resp.StatusCode, shortDuration(wait)), wait)
	case resp.StatusCode != http.StatusOK:
		return 0, in.recordFailure(ctx, st.ID, fmt.Sprintf("HTTP %d", resp.StatusCode), 0)
	}

	items, err := Parse(resp.Body)
	if err != nil {
		return 0, in.recordFailure(ctx, st.ID, err.Error(), 0)
	}

	now := time.Now()
	fresh := selectNew(items, st.CursorAt, st.LastGUIDs, st.LastFetchAt == nil, now)
	guids := make([]string, len(items))
	for i, it := range items {
		guids[i] = it.GUIDHash
	}

	inserted := 0
	err = pgx.BeginFunc(ctx, in.pool, func(tx pgx.Tx) error {
		for _, it := range fresh {
			tag, err := tx.Exec(ctx, `
				INSERT INTO raw_items (source_id, guid_hash, url, title, snippet, published_at)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (source_id, guid_hash) DO NOTHING`,
				st.ID, it.GUIDHash, it.URL, it.Title, it.Snippet, it.PublishedAt)
			if err != nil {
				return fmt.Errorf("insert item: %w", err)
			}
			inserted += int(tag.RowsAffected())
		}
		_, err := tx.Exec(ctx, `
			UPDATE sources SET
				etag = $2, last_modified = $3, cursor_at = $4, last_guids = $5,
				last_fetch_at = now(), last_status = $6, consecutive_failures = 0, retry_after = NULL
			WHERE id = $1`,
			st.ID, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"),
			nextCursor(items, st.CursorAt, now), guids, fmt.Sprintf("ok: %d items, %d new", len(items), len(fresh)))
		return err
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

func (in *Ingester) recordFailure(ctx context.Context, id int64, reason string, wait time.Duration) error {
	if len(reason) > maxStoredStatusText {
		reason = reason[:maxStoredStatusText]
	}
	var failures int
	err := in.pool.QueryRow(ctx, `
		UPDATE sources SET last_fetch_at = now(), last_status = $2, consecutive_failures = consecutive_failures + 1,
			retry_after = CASE WHEN $3 > 0 THEN now() + make_interval(secs => $3) END
		WHERE id = $1 RETURNING consecutive_failures`, id, reason, wait.Seconds()).Scan(&failures)
	if err == nil && failures >= maxFailuresInARow {
		_, err = in.pool.Exec(ctx, "UPDATE sources SET enabled = false, last_status = $2 WHERE id = $1",
			id, fmt.Sprintf("disabled after %d failures in a row: %s", failures, reason))
	}
	if err != nil {
		return fmt.Errorf("%s (and recording it failed: %w)", reason, err)
	}
	return errors.New(reason)
}

func (in *Ingester) Preview(ctx context.Context, feedURL, userAgent string) ([]Item, error) {
	header := http.Header{}
	if userAgent != "" {
		header.Set("User-Agent", userAgent)
	}
	resp, err := in.client.Get(ctx, feedURL, header)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return Parse(resp.Body)
}

func backoff(header http.Header, failures int, now time.Time) time.Duration {
	wait := firstBackoff << min(failures, 10)
	if v := strings.TrimSpace(header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			wait = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(v); err == nil {
			wait = t.Sub(now)
		}
	}
	return min(max(wait, 0), maxBackoff)
}

func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	s := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}
