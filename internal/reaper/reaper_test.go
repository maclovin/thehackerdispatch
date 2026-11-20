package reaper

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

func TestRunOnce(t *testing.T) {
	pool := storetest.New(t)
	ctx := context.Background()
	storetest.MustExec(t, pool, `INSERT INTO sources (name, url, section_id) VALUES ('S', 'https://example.com/feed', 1)`)
	storetest.MustExec(t, pool, `INSERT INTO raw_items (source_id, guid_hash, url, title, fetched_at) VALUES
		(1, 'old', 'https://example.com/1', 'old', now() - interval '49 hours'),
		(1, 'new', 'https://example.com/2', 'new', now() - interval '47 hours')`)
	storetest.MustExec(t, pool, `INSERT INTO audit_log (actor_label, action, created_at) VALUES
		('system', 'old', now() - interval '91 days'), ('system', 'new', now())`)
	storetest.MustExec(t, pool, `INSERT INTO stories (title, section_id, status, created_at, published_at, expires_at) VALUES
		('queued yesterday', 1, 'queued', date_trunc('day', now()) - interval '1 minute', NULL, NULL),
		('queued today', 1, 'queued', date_trunc('day', now()), NULL, NULL),
		('expired', 1, 'published', now() - interval '2 days', now() - interval '2 days', now() - interval '1 hour'),
		('forever', 1, 'published', now() - interval '30 days', now() - interval '30 days', NULL)`)
	storetest.MustExec(t, pool, `INSERT INTO slack_outbox (story_id, status, created_at)
		SELECT id, s.status, s.created_at FROM stories, (VALUES
			('sent', now() - interval '8 days'), ('failed', now() - interval '8 days'),
			('sent', now() - interval '6 days'), ('pending', now() - interval '8 days')) AS s(status, created_at)
		WHERE title = 'forever'`)
	storetest.MustExec(t, pool, `INSERT INTO pacing_arrivals (arrived_at, score) VALUES (now() - interval '29 days', 50), (now() - interval '27 days', 50)`)

	if err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil))).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var items, entries int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM raw_items), (SELECT count(*) FROM audit_log)").Scan(&items, &entries); err != nil {
		t.Fatal(err)
	}
	if items != 1 || entries != 1 {
		t.Errorf("left %d raw items and %d audit entries, want 1 and 1", items, entries)
	}
	var stories string
	var messages, arrivals int
	if err := pool.QueryRow(ctx, `SELECT (SELECT string_agg(title, ',' ORDER BY id) FROM stories),
		(SELECT count(*) FROM slack_outbox), (SELECT count(*) FROM pacing_arrivals)`).Scan(&stories, &messages, &arrivals); err != nil {
		t.Fatal(err)
	}
	if stories != "queued today,forever" || messages != 2 || arrivals != 1 {
		t.Errorf("left stories %q, %d Slack messages and %d arrivals, want \"queued today,forever\", 2 and 1", stories, messages, arrivals)
	}
}
