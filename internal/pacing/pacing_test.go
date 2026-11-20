package pacing

import (
	"context"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/settings"
	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

func TestExpected(t *testing.T) {
	wed := func(hour, minute int) time.Time { return time.Date(2026, 10, 21, hour, minute, 0, 0, time.UTC) }
	tests := []struct {
		name string
		plan Plan
		want float64
	}{
		{"no history", Plan{Now: wed(10, 0)}, 0},
		{
			"less than a week: the average rate",
			Plan{Now: wed(18, 0), History: 48 * time.Hour, Arrivals: map[Hour]int{{time.Monday, 3}: 90, {time.Tuesday, 9}: 6}},
			12,
		},
		{
			"same weekday and hours",
			Plan{Now: wed(18, 30), History: 40 * 24 * time.Hour, Arrivals: map[Hour]int{
				{time.Wednesday, 18}: 8, {time.Wednesday, 19}: 4,
				{time.Wednesday, 10}: 40,
				{time.Thursday, 20}:  100,
			}},
			2,
		},
	}
	for _, tt := range tests {
		if got := tt.plan.Expected(); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s: Expected() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestThreshold(t *testing.T) {
	noon := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	hundred := make([]int, 100)
	for i := range hundred {
		hundred[i] = i
	}
	tests := []struct {
		name string
		plan Plan
		want int
	}{
		{"enough room for everything", Plan{Now: noon, Budget: 10, Queued: 3, MinScore: 40}, 40},
		{"uniform scores until there are 50", Plan{Now: noon, Budget: 2, Queued: 8, MinScore: 40, Scores: []int{90, 95}}, 75},
		{"recent scores", Plan{Now: noon, Budget: 2, Queued: 8, MinScore: 40, Scores: hundred}, 75},
		{"never under the minimum score", Plan{Now: noon, Budget: 2, Queued: 8, MinScore: 80, Scores: hundred}, 80},
		{
			"forecast",
			Plan{Now: noon, Budget: 8, Queued: 3, MinScore: 40, History: 24 * time.Hour, Arrivals: map[Hour]int{{time.Tuesday, 9}: 50}},
			71,
		},
	}
	for _, tt := range tests {
		if got := tt.plan.Threshold(); got != tt.want {
			t.Errorf("%s: Threshold() = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestGap(t *testing.T) {
	day := float64(24 * time.Hour)
	tests := []struct {
		left   time.Duration
		budget int
		jitter float64
		want   time.Duration
	}{
		{24 * time.Hour, 10, 1, 24 * time.Hour / 11},
		{24 * time.Hour, 10, 1.15, time.Duration(day / 11 * 1.15)},
		{2 * time.Hour, 10, 1, 30 * time.Minute},
		{5 * time.Hour, 0, 1, 5 * time.Hour},
	}
	for _, tt := range tests {
		if got := Gap(tt.left, tt.budget, 30*time.Minute, tt.jitter); got != tt.want {
			t.Errorf("Gap(%v, %d, 30m, %v) = %v, want %v", tt.left, tt.budget, tt.jitter, got, tt.want)
		}
	}
}

type fakePublisher struct {
	pool      *pgxpool.Pool
	published []int64
}

func (f *fakePublisher) Publish(ctx context.Context, id int64) error {
	f.published = append(f.published, id)
	_, err := f.pool.Exec(ctx, "UPDATE stories SET status = 'published', published_at = now() WHERE id = $1 AND status = 'queued'", id)
	return err
}

func setup(t *testing.T, content settings.Content) (*Pacer, *fakePublisher, *pgxpool.Pool) {
	pool := storetest.New(t)
	st := settings.New(pool)
	if err := st.SetContent(context.Background(), content, audit.Actor{Label: "test"}); err != nil {
		t.Fatal(err)
	}
	pub := &fakePublisher{pool: pool}
	p := New(pool, st, pub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.random = func() float64 { return 0.5 }
	return p, pub, pool
}

func addStory(t *testing.T, pool *pgxpool.Pool, title string, score int, priority, created string) int64 {
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO stories (title, section_id, score, priority, created_at) VALUES ($1, 1, $2, $3, `+created+`)
		RETURNING id`, title, score, priority).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func titles(t *testing.T, pool *pgxpool.Pool, ids []int64) string {
	var out []string
	for _, id := range ids {
		var title string
		if err := pool.QueryRow(context.Background(), "SELECT title FROM stories WHERE id = $1", id).Scan(&title); err != nil {
			t.Fatal(err)
		}
		out = append(out, title)
	}
	return strings.Join(out, ",")
}

func nextSlotIn(t *testing.T, pool *pgxpool.Pool) time.Duration {
	var d time.Duration
	if err := pool.QueryRow(context.Background(), "SELECT next_slot_at - now() FROM pacing_state").Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRunOnce(t *testing.T) {
	p, pub, pool := setup(t, settings.Content{TTLDays: 7, DailyCap: 2, MinScore: 50, MinGapMinutes: 30})
	ctx := context.Background()
	addStory(t, pool, "low", 30, "normal", "now()")
	addStory(t, pool, "good 1", 90, "normal", "now()")
	addStory(t, pool, "good 2", 80, "normal", "now()")
	addStory(t, pool, "critical", 10, "critical", "now()")
	addStory(t, pool, "from yesterday", 99, "normal", "date_trunc('day', now()) - interval '1 minute'")

	n, err := p.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || titles(t, pool, pub.published) != "critical,good 1" {
		t.Errorf("published %d: %s", n, titles(t, pool, pub.published))
	}
	var tomorrow time.Duration
	if err := pool.QueryRow(ctx, "SELECT next_slot_at - (date_trunc('day', now()) + interval '1 day') FROM pacing_state").Scan(&tomorrow); err != nil {
		t.Fatal(err)
	}
	if tomorrow.Abs() > time.Minute {
		t.Errorf("next slot is %v away from midnight", tomorrow)
	}

	storetest.MustExec(t, pool, "UPDATE pacing_state SET next_slot_at = now() - interval '1 minute'")
	urgent := addStory(t, pool, "critical 2", 10, "critical", "now()")
	if n, err := p.RunOnce(ctx); err != nil || n != 1 || pub.published[len(pub.published)-1] != urgent {
		t.Errorf("over the cap: published %d (%v): %s", n, err, titles(t, pool, pub.published))
	}
}

func TestSlotWaitsForBetterStories(t *testing.T) {
	p, pub, pool := setup(t, settings.Content{TTLDays: 7, DailyCap: 1, MinScore: 50, MinGapMinutes: 30})
	ctx := context.Background()
	addStory(t, pool, "fair 1", 60, "normal", "now()")
	addStory(t, pool, "fair 2", 55, "normal", "now()")
	addStory(t, pool, "fair 3", 50, "normal", "now()")

	if n, err := p.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("published %d (%v), want 0", n, err)
	}
	if d := nextSlotIn(t, pool); d < 29*time.Minute {
		t.Errorf("the next slot is in %v, under the minimum gap", d)
	}
	if n, _ := p.RunOnce(ctx); n != 0 {
		t.Error("published before the next slot")
	}

	storetest.MustExec(t, pool, "INSERT INTO pacing_arrivals (arrived_at, score) SELECT now() - interval '1 hour', 40 FROM generate_series(1, 60)")
	storetest.MustExec(t, pool, "UPDATE pacing_state SET next_slot_at = now()")
	if n, err := p.RunOnce(ctx); err != nil || n != 1 || titles(t, pool, pub.published) != "fair 1" {
		t.Errorf("published %d (%v): %s", n, err, titles(t, pool, pub.published))
	}
}
