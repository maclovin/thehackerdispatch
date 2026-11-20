package reaper

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Reaper struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

func New(pool *pgxpool.Pool, logger *slog.Logger) *Reaper {
	return &Reaper{pool: pool, logger: logger}
}

var rules = []struct {
	name string
	sql  string
}{
	{"expired stories", "DELETE FROM stories WHERE expires_at < now()"},
	{"stories queued on earlier days", "DELETE FROM stories WHERE status = 'queued' AND created_at < date_trunc('day', now())"},
	{"Slack messages done over 7 days ago", "DELETE FROM slack_outbox WHERE status <> 'pending' AND created_at < now() - interval '7 days'"},
	{"pacing arrivals older than 4 weeks", "DELETE FROM pacing_arrivals WHERE arrived_at < now() - interval '28 days'"},
	{"raw items older than 48h", "DELETE FROM raw_items WHERE fetched_at < now() - interval '48 hours'"},
	{"expired sessions", "DELETE FROM sessions WHERE expires_at < now()"},
	{"model calls older than 30 days", "DELETE FROM llm_calls WHERE created_at < now() - interval '30 days'"},
	{"audit entries older than 90 days", "DELETE FROM audit_log WHERE created_at < now() - interval '90 days'"},
}

func (r *Reaper) RunOnce(ctx context.Context) error {
	for _, rule := range rules {
		tag, err := r.pool.Exec(ctx, rule.sql)
		if err != nil {
			return fmt.Errorf("delete %s: %w", rule.name, err)
		}
		if n := tag.RowsAffected(); n > 0 {
			r.logger.Info("deleted expired data", "what", rule.name, "rows", n)
		}
	}
	return nil
}
