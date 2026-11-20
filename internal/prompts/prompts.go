package prompts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

const Global int64 = 0

const MaxLength = 20000

const historyLimit = 20

const DefaultGlobal = `Audience: software engineers, SREs and security people at a technology company. Security is the main topic; tech, business and engineering news are welcome when they matter to that audience.

What to publish
- Score high (70-100): actively exploited vulnerabilities; critical vulnerabilities in widely used software, cloud services, network appliances or developer tooling; major breaches and ransomware incidents with technical detail; supply chain attacks (malicious packages, compromised CI/CD); significant new threat actor tradecraft; major platform or infrastructure outages with lessons learned.
- Score medium (40-69): notable vulnerability research, security tooling releases, important policy or regulation changes, relevant industry news.
- Score low or discard: product marketing, webinars, listicles, vague trend pieces, routine patch notes without security impact, stories about consumer gadgets.

Mark as critical only: a vulnerability or campaign being exploited right now in software commonly run by companies (VPNs, firewalls, identity providers, CI/CD, cloud consoles, popular libraries), or a breach of a major vendor that customers must act on today.

How to write
Be concise and concrete. For vulnerabilities, name the affected products and versions, the CVE ids, whether exploitation has been seen, and whether a fix or mitigation exists. For incidents, say who was affected, what happened, and what is known about the attacker. Prefer plain language to jargon and marketing terms.`

var (
	ErrNotFound    = errors.New("prompt version not found")
	ErrEmptyGlobal = errors.New("the global prompt can't be empty")
	ErrTooLong     = fmt.Errorf("a prompt is limited to %d characters", MaxLength)
	ErrConflict    = errors.New("someone else saved this prompt at the same time; reload and try again")
)

type Version struct {
	ID        int64
	Version   int
	Body      string
	Author    string
	CreatedAt time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) For(ctx context.Context, sectionID int64) (global, section string, err error) {
	var g, sec *string
	err = s.pool.QueryRow(ctx, `
		SELECT (SELECT body FROM prompts WHERE section_id IS NULL ORDER BY version DESC LIMIT 1),
		       (SELECT body FROM prompts WHERE section_id = $1 ORDER BY version DESC LIMIT 1)`,
		sectionID).Scan(&g, &sec)
	if err != nil {
		return DefaultGlobal, "", fmt.Errorf("read prompts: %w", err)
	}
	global = DefaultGlobal
	if g != nil {
		global = *g
	}
	if sec != nil {
		section = *sec
	}
	return global, section, nil
}

func (s *Store) Current(ctx context.Context, scope int64) (Version, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, version, body, author, created_at FROM prompts
		WHERE section_id IS NOT DISTINCT FROM $1 ORDER BY version DESC LIMIT 1`, sectionArg(scope))
	if err != nil {
		return Version{}, fmt.Errorf("read prompt: %w", err)
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Version])
	if errors.Is(err, pgx.ErrNoRows) {
		if scope == Global {
			return Version{Body: DefaultGlobal}, nil
		}
		return Version{}, nil
	}
	return v, err
}

func (s *Store) History(ctx context.Context, scope int64) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, version, body, author, created_at FROM prompts
		WHERE section_id IS NOT DISTINCT FROM $1 ORDER BY version DESC LIMIT $2`, sectionArg(scope), historyLimit)
	if err != nil {
		return nil, fmt.Errorf("list prompt versions: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Version])
}

func (s *Store) Save(ctx context.Context, scope int64, body string, actor audit.Actor) (bool, error) {
	body = normalize(body)
	switch {
	case scope == Global && body == "":
		return false, ErrEmptyGlobal
	case len([]rune(body)) > MaxLength:
		return false, ErrTooLong
	}
	current, err := s.Current(ctx, scope)
	if err != nil {
		return false, err
	}
	if current.Body == body {
		return false, nil
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var version int
		err := tx.QueryRow(ctx, `
			INSERT INTO prompts (section_id, version, body, author)
			SELECT $1, coalesce(max(version), 0) + 1, $2, $3 FROM prompts WHERE section_id IS NOT DISTINCT FROM $1
			RETURNING version`, sectionArg(scope), body, actor.Label).Scan(&version)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrConflict
			}
			return fmt.Errorf("save prompt: %w", err)
		}
		return audit.Record(ctx, tx, actor.Entry("prompt.saved", target(scope), map[string]any{"version": version}))
	})
	return err == nil, err
}

func (s *Store) Restore(ctx context.Context, id int64, actor audit.Actor) (int64, error) {
	var (
		sectionID *int64
		body      string
	)
	err := s.pool.QueryRow(ctx, "SELECT section_id, body FROM prompts WHERE id = $1", id).Scan(&sectionID, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	scope := Global
	if sectionID != nil {
		scope = *sectionID
	}
	_, err = s.Save(ctx, scope, body, actor)
	return scope, err
}

func normalize(body string) string {
	return strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
}

func sectionArg(scope int64) *int64 {
	if scope == Global {
		return nil
	}
	return &scope
}

func target(scope int64) string {
	if scope == Global {
		return "prompt:global"
	}
	return fmt.Sprintf("prompt:section:%d", scope)
}
