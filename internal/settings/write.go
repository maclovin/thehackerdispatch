package settings

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

func (s *Store) set(ctx context.Context, key string, value any, actor audit.Actor) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode setting %q: %w", key, err)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var updatedBy *int64
		if actor.ID != 0 {
			updatedBy = &actor.ID
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO settings (key, value, updated_by, updated_at) VALUES ($1, $2, $3, now())
			ON CONFLICT (key) DO UPDATE SET value = $2, updated_by = $3, updated_at = now()`,
			key, raw, updatedBy)
		if err != nil {
			return fmt.Errorf("save setting %q: %w", key, err)
		}
		return audit.Record(ctx, tx, actor.Entry("settings.updated", "settings:"+key, map[string]any{"value": json.RawMessage(raw)}))
	})
}

func (s *Store) SetIngest(ctx context.Context, v Ingest, actor audit.Actor) error {
	return s.set(ctx, "ingest", v, actor)
}

func (s *Store) SetContent(ctx context.Context, v Content, actor audit.Actor) error {
	return s.set(ctx, "content", v, actor)
}

func (s *Store) SetAuth(ctx context.Context, v Auth, actor audit.Actor) error {
	return s.set(ctx, "auth", v, actor)
}

func (s *Store) SetBranding(ctx context.Context, v Branding, actor audit.Actor) error {
	return s.set(ctx, "branding", v, actor)
}

func (s *Store) SetLLMStages(ctx context.Context, v LLMStages, actor audit.Actor) error {
	return s.set(ctx, "llm_stages", v, actor)
}
