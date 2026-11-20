package pacing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/settings"
)

const JobName = "pacing"

const (
	historyWeeks = 4
	minScores    = 50
	jitter       = 0.15
)

type Publisher interface {
	Publish(ctx context.Context, id int64) error
}

type Pacer struct {
	pool      *pgxpool.Pool
	settings  *settings.Store
	publisher Publisher
	logger    *slog.Logger
	random    func() float64
}

func New(pool *pgxpool.Pool, settings *settings.Store, publisher Publisher, logger *slog.Logger) *Pacer {
	return &Pacer{pool: pool, settings: settings, publisher: publisher, logger: logger, random: rand.Float64}
}

func (p *Pacer) RunOnce(ctx context.Context) (int, error) {
	// Only one replica runs this job, so anything still "publishing" was interrupted.
	if _, err := p.pool.Exec(ctx, "UPDATE stories SET status = 'queued' WHERE status = 'publishing'"); err != nil {
		return 0, err
	}
	published := 0
	rows, err := p.pool.Query(ctx, "SELECT id FROM stories WHERE status = 'queued' AND priority = 'critical' ORDER BY created_at")
	if err != nil {
		return 0, err
	}
	critical, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	for _, id := range critical {
		if err := p.publisher.Publish(ctx, id); err != nil {
			if ctx.Err() != nil {
				return published, ctx.Err()
			}
			p.logger.Error("publish critical story", "story", id, "err", err)
			continue
		}
		published++
	}

	content, err := p.settings.Content(ctx)
	if err != nil {
		p.logger.Error("load content settings, using defaults", "err", err)
	}
	plan := Plan{MinScore: content.MinScore}
	var (
		publishedToday int
		waiting        bool
	)
	err = p.pool.QueryRow(ctx, `
		SELECT localtimestamp,
		       (SELECT count(*) FROM stories WHERE status = 'published' AND published_at >= date_trunc('day', now())),
		       (SELECT count(*) FROM stories WHERE status = 'queued' AND priority = 'normal'
		        AND created_at >= date_trunc('day', now())),
		       coalesce((SELECT next_slot_at > now() FROM pacing_state), false)`).
		Scan(&plan.Now, &publishedToday, &plan.Queued, &waiting)
	if err != nil {
		return published, err
	}
	plan.Budget = content.DailyCap - publishedToday
	if waiting || plan.Budget <= 0 {
		return published, nil
	}

	if err := p.loadHistory(ctx, &plan); err != nil {
		return published, err
	}
	threshold := plan.Threshold()
	var best struct {
		ID    int64
		Score int
	}
	err = p.pool.QueryRow(ctx, `
		SELECT id, score FROM stories WHERE status = 'queued' AND priority = 'normal'
		AND created_at >= date_trunc('day', now())
		ORDER BY score DESC, created_at LIMIT 1`).Scan(&best.ID, &best.Score)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return published, err
	}
	outcome := "nothing queued"
	switch {
	case best.ID == 0:
	case best.Score < threshold:
		outcome = "best story below the threshold"
	default:
		if err := p.publisher.Publish(ctx, best.ID); err != nil {
			if ctx.Err() != nil {
				return published, ctx.Err()
			}
			p.logger.Error("publish story", "story", best.ID, "err", err)
			outcome = "publishing failed"
			break
		}
		outcome = "published"
		published++
		plan.Budget--
	}

	gap := Gap(plan.Left(), plan.Budget, content.MinGap(), 1+jitter*(2*p.random()-1))
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO pacing_state (next_slot_at) VALUES (now() + make_interval(secs => $1))
		ON CONFLICT (id) DO UPDATE SET next_slot_at = excluded.next_slot_at`, gap.Seconds()); err != nil {
		return published, fmt.Errorf("schedule the next slot: %w", err)
	}
	p.logger.Info("pacing slot", "outcome", outcome, "story", best.ID, "score", best.Score, "threshold", threshold,
		"queued", plan.Queued, "expected", math.Round(plan.Expected()*10)/10, "budget_left", plan.Budget,
		"next_slot_in", gap.Round(time.Minute).String())
	return published, nil
}

func (p *Pacer) loadHistory(ctx context.Context, plan *Plan) error {
	var seconds *float64
	if err := p.pool.QueryRow(ctx, "SELECT extract(epoch FROM now() - min(arrived_at))::float8 FROM pacing_arrivals").
		Scan(&seconds); err != nil {
		return fmt.Errorf("arrival history: %w", err)
	}
	if seconds != nil {
		plan.History = time.Duration(*seconds * float64(time.Second))
	}
	rows, err := p.pool.Query(ctx, `
		SELECT extract(isodow FROM arrived_at)::int % 7, extract(hour FROM arrived_at)::int, count(*)
		FROM pacing_arrivals WHERE arrived_at > now() - make_interval(weeks => $1)
		GROUP BY 1, 2`, historyWeeks)
	if err != nil {
		return fmt.Errorf("arrivals: %w", err)
	}
	plan.Arrivals = map[Hour]int{}
	var weekday, hour, n int
	_, err = pgx.ForEachRow(rows, []any{&weekday, &hour, &n}, func() error {
		plan.Arrivals[Hour{time.Weekday(weekday), hour}] = n
		return nil
	})
	if err != nil {
		return fmt.Errorf("arrivals: %w", err)
	}
	rows, err = p.pool.Query(ctx, `
		SELECT score FROM pacing_arrivals WHERE arrived_at > now() - make_interval(weeks => $1)
		ORDER BY score`, historyWeeks)
	if err != nil {
		return fmt.Errorf("scores: %w", err)
	}
	plan.Scores, err = pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return fmt.Errorf("scores: %w", err)
	}
	return nil
}

type Hour struct {
	Weekday time.Weekday
	Hour    int
}

type Plan struct {
	Now      time.Time
	Budget   int
	Queued   int
	Arrivals map[Hour]int
	History  time.Duration
	Scores   []int
	MinScore int
}

func (p Plan) Left() time.Duration {
	y, m, d := p.Now.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, p.Now.Location()).Sub(p.Now)
}

func (p Plan) Expected() float64 {
	if p.History <= 0 {
		return 0
	}
	weeks := p.History.Hours() / (7 * 24)
	if weeks < 1 {
		total := 0
		for _, n := range p.Arrivals {
			total += n
		}
		return float64(total) / max(p.History.Hours(), 24) * p.Left().Hours()
	}
	weeks = min(weeks, historyWeeks)
	expected := 0.0
	for h := p.Now.Hour(); h < 24; h++ {
		n := float64(p.Arrivals[Hour{p.Now.Weekday(), h}]) / weeks
		if h == p.Now.Hour() {
			n *= 1 - float64(p.Now.Minute()*60+p.Now.Second())/3600
		}
		expected += n
	}
	return expected
}

func (p Plan) Threshold() int {
	n := float64(p.Queued) + p.Expected()
	if n <= float64(p.Budget) {
		return p.MinScore
	}
	return max(quantile(p.Scores, 1-float64(p.Budget)/n), p.MinScore)
}

func quantile(sorted []int, q float64) int {
	if len(sorted) < minScores {
		return int(math.Round(100 * q))
	}
	return sorted[min(int(q*float64(len(sorted))), len(sorted)-1)]
}

func Gap(left time.Duration, budget int, minGap time.Duration, jitter float64) time.Duration {
	if budget <= 0 {
		return left
	}
	return max(time.Duration(float64(left)/float64(budget+1)*jitter), minGap)
}
