package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/fetch"
	"github.com/maclovin/thehackerdispatch/internal/sources"
	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type feedServer struct {
	mu    sync.Mutex
	items []string
	etag  int
}

func (f *feedServer) set(items ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = items
	f.etag++
}

func (f *feedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	etag := fmt.Sprintf(`"%d"`, f.etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>T</title>`)
	for _, it := range f.items {
		fmt.Fprintf(&b, `<item><title>%[1]s</title><link>https://example.com/%[1]s</link><guid>%[1]s</guid><pubDate>%[2]s</pubDate></item>`,
			it, time.Now().UTC().Format(time.RFC1123))
	}
	b.WriteString(`</channel></rss>`)
	io.WriteString(w, b.String())
}

func newIngester(t *testing.T) (*Ingester, *pgxpool.Pool) {
	pool := storetest.New(t)
	return New(pool, fetch.New(fetch.Options{UserAgent: "test", AllowPrivate: true}), discard), pool
}

func addSource(t *testing.T, pool *pgxpool.Pool, url string) int64 {
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO sources (name, url, section_id) VALUES ('Test', $1, (SELECT id FROM sections WHERE slug = 'security'))
		RETURNING id`, url).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func countItems(t *testing.T, pool *pgxpool.Pool) int {
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM raw_items").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunOnce(t *testing.T) {
	in, pool := newIngester(t)
	feed := &feedServer{}
	feed.set("a", "b")
	srv := httptest.NewServer(feed)
	defer srv.Close()
	addSource(t, pool, srv.URL)
	ctx := context.Background()

	stats, err := in.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewItems != 2 || countItems(t, pool) != 2 {
		t.Fatalf("first run: stats = %+v, stored = %d", stats, countItems(t, pool))
	}

	if stats, _ := in.RunOnce(ctx); stats.NewItems != 0 {
		t.Errorf("unchanged feed: %d new items", stats.NewItems)
	}

	feed.set("c", "a", "b")
	if stats, _ := in.RunOnce(ctx); stats.NewItems != 1 || countItems(t, pool) != 3 {
		t.Errorf("after adding an item: stats = %+v, stored = %d", stats, countItems(t, pool))
	}

	var status string
	if err := pool.QueryRow(ctx, "SELECT last_status FROM sources").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ok: 3 items, 1 new" {
		t.Errorf("last_status = %q", status)
	}
}

func TestFailingSourceIsDisabled(t *testing.T) {
	in, pool := newIngester(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	id := addSource(t, pool, srv.URL)
	ctx := context.Background()

	for range maxFailuresInARow {
		if stats, _ := in.RunOnce(ctx); stats.Failed != 1 {
			t.Fatalf("stats = %+v, want 1 failure", stats)
		}
	}
	var (
		enabled bool
		status  string
	)
	if err := pool.QueryRow(ctx, "SELECT enabled, last_status FROM sources WHERE id = $1", id).Scan(&enabled, &status); err != nil {
		t.Fatal(err)
	}
	if enabled || !strings.HasPrefix(status, "disabled after 10 failures in a row: HTTP 500") {
		t.Errorf("enabled = %v, status = %q", enabled, status)
	}
	if stats, _ := in.RunOnce(ctx); stats.Sources != 0 {
		t.Errorf("disabled source still fetched")
	}
}

func TestTooManyRequestsWaits(t *testing.T) {
	in, pool := newIngester(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7200")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	id := addSource(t, pool, srv.URL)
	ctx := context.Background()

	if stats, _ := in.RunOnce(ctx); stats.Failed != 1 {
		t.Fatalf("stats = %+v, want 1 failure", stats)
	}
	var (
		status string
		wait   time.Duration
	)
	if err := pool.QueryRow(ctx, "SELECT last_status, retry_after - now() FROM sources WHERE id = $1", id).Scan(&status, &wait); err != nil {
		t.Fatal(err)
	}
	if status != "HTTP 429, next try in 2h" || wait < 119*time.Minute || wait > 2*time.Hour {
		t.Errorf("status = %q, wait = %v", status, wait)
	}
	if stats, _ := in.RunOnce(ctx); stats.Sources != 0 {
		t.Error("the source was fetched again before Retry-After")
	}

	store, actor := sources.New(pool), audit.Actor{Label: "test"}
	if err := store.SetEnabled(ctx, id, false, actor); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(ctx, id, true, actor); err != nil {
		t.Fatal(err)
	}
	if stats, _ := in.RunOnce(ctx); stats.Sources != 1 {
		t.Error("the source should be fetched once the wait is over")
	}
}

func TestBackoff(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	header := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return h
	}
	tests := []struct {
		retryAfter string
		failures   int
		want       time.Duration
	}{
		{"", 0, 15 * time.Minute},
		{"", 1, 30 * time.Minute},
		{"", 3, 2 * time.Hour},
		{"", 9, 24 * time.Hour},
		{"90", 5, 90 * time.Second},
		{"Wed, 21 Oct 2026 13:30:00 GMT", 0, 90 * time.Minute},
		{"Wed, 21 Oct 2026 11:00:00 GMT", 0, 0},
		{"999999", 0, 24 * time.Hour},
		{"soon", 0, 15 * time.Minute},
	}
	for _, tt := range tests {
		if got := backoff(header(tt.retryAfter), tt.failures, now); got != tt.want {
			t.Errorf("backoff(%q, %d) = %v, want %v", tt.retryAfter, tt.failures, got, tt.want)
		}
	}
	for d, want := range map[time.Duration]string{45 * time.Second: "45s", 15 * time.Minute: "15m",
		2 * time.Hour: "2h", 150 * time.Minute: "2h30m", 24 * time.Hour: "24h"} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
