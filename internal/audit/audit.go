package audit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Actor struct {
	ID    int64
	Label string
}

func (a Actor) Entry(action, target string, details map[string]any) Entry {
	return Entry{ActorID: a.ID, ActorLabel: a.Label, Action: action, Target: target, Details: details}
}

type Entry struct {
	ActorID    int64
	ActorLabel string
	Action     string
	Target     string
	Details    map[string]any
}

func Record(ctx context.Context, db Execer, e Entry) error {
	var actorID *int64
	if e.ActorID != 0 {
		actorID = &e.ActorID
	}
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	_, err := db.Exec(ctx, `
		INSERT INTO audit_log (actor_id, actor_label, action, target, details)
		VALUES ($1, $2, $3, $4, $5)`,
		actorID, e.ActorLabel, e.Action, e.Target, details)
	if err != nil {
		return fmt.Errorf("record audit entry %q: %w", e.Action, err)
	}
	return nil
}
