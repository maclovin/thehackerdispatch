package sources

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

var (
	ErrNotFound       = errors.New("source not found")
	ErrDuplicateURL   = errors.New("a source with this URL already exists")
	ErrUnknownSection = errors.New("unknown section")
)

type Section struct {
	ID   int64
	Slug string
	Name string
	Sort int
}

type Source struct {
	ID                  int64
	Name                string
	URL                 string
	SectionID           int64
	SectionName         string
	Enabled             bool
	UserAgent           string
	LastFetchAt         *time.Time
	LastStatus          string
	ConsecutiveFailures int
	ItemsLast48h        int
}

type Input struct {
	Name      string
	URL       string
	SectionID int64
	Enabled   bool
	UserAgent string
}

func (in *Input) Validate() error {
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	in.UserAgent = strings.TrimSpace(in.UserAgent)
	var errs []error
	if in.Name == "" || len(in.Name) > 100 {
		errs = append(errs, errors.New("name must have 1 to 100 characters"))
	}
	if u, err := url.Parse(in.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("URL must be an absolute http(s) URL"))
	}
	if in.SectionID == 0 {
		errs = append(errs, errors.New("pick a section"))
	}
	if len(in.UserAgent) > 300 {
		errs = append(errs, errors.New("User-Agent is too long"))
	}
	return errors.Join(errs...)
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Sections(ctx context.Context) ([]Section, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, slug, name, sort FROM sections ORDER BY sort, name")
	if err != nil {
		return nil, fmt.Errorf("list sections: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Section])
}

const sourceColumns = `
	s.id, s.name, s.url, s.section_id, sec.name, s.enabled, s.user_agent,
	s.last_fetch_at, s.last_status, s.consecutive_failures,
	(SELECT count(*) FROM raw_items r WHERE r.source_id = s.id)`

func (s *Store) List(ctx context.Context) ([]Source, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sourceColumns+`
		FROM sources s JOIN sections sec ON sec.id = s.section_id
		ORDER BY sec.sort, lower(s.name)`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Source])
}

func (s *Store) Get(ctx context.Context, id int64) (Source, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sourceColumns+`
		FROM sources s JOIN sections sec ON sec.id = s.section_id
		WHERE s.id = $1`, id)
	if err != nil {
		return Source{}, fmt.Errorf("get source: %w", err)
	}
	src, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Source])
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	return src, err
}

func (s *Store) Create(ctx context.Context, in Input, actor audit.Actor) (int64, error) {
	if err := in.Validate(); err != nil {
		return 0, err
	}
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO sources (name, url, section_id, enabled, user_agent)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			in.Name, in.URL, in.SectionID, in.Enabled, in.UserAgent).Scan(&id)
		if err != nil {
			return mapError(err)
		}
		return audit.Record(ctx, tx, actor.Entry("source.created", target(id), map[string]any{"name": in.Name, "url": in.URL}))
	})
	return id, err
}

func (s *Store) Update(ctx context.Context, id int64, in Input, actor audit.Actor) error {
	if err := in.Validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE sources SET
				name = $2, section_id = $4, enabled = $5, user_agent = $6,
				consecutive_failures = CASE WHEN $5 AND NOT enabled THEN 0 ELSE consecutive_failures END,
				retry_after   = CASE WHEN url = $3 AND NOT ($5 AND NOT enabled) THEN retry_after END,
				etag          = CASE WHEN url = $3 THEN etag ELSE '' END,
				last_modified = CASE WHEN url = $3 THEN last_modified ELSE '' END,
				cursor_at     = CASE WHEN url = $3 THEN cursor_at END,
				last_guids    = CASE WHEN url = $3 THEN last_guids ELSE '{}' END,
				last_fetch_at = CASE WHEN url = $3 THEN last_fetch_at END,
				url = $3
			WHERE id = $1`,
			id, in.Name, in.URL, in.SectionID, in.Enabled, in.UserAgent)
		if err != nil {
			return mapError(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return audit.Record(ctx, tx, actor.Entry("source.updated", target(id), map[string]any{"name": in.Name, "url": in.URL}))
	})
}

func (s *Store) SetEnabled(ctx context.Context, id int64, enabled bool, actor audit.Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE sources SET enabled = $2,
				consecutive_failures = CASE WHEN $2 THEN 0 ELSE consecutive_failures END,
				retry_after = CASE WHEN $2 THEN NULL ELSE retry_after END
			WHERE id = $1`, id, enabled)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		action := "source.disabled"
		if enabled {
			action = "source.enabled"
		}
		return audit.Record(ctx, tx, actor.Entry(action, target(id), nil))
	})
}

func (s *Store) Delete(ctx context.Context, id int64, actor audit.Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var name string
		err := tx.QueryRow(ctx, "DELETE FROM sources WHERE id = $1 RETURNING name", id).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Entry("source.deleted", target(id), map[string]any{"name": name}))
	})
}

func target(id int64) string { return fmt.Sprintf("source:%d", id) }

func mapError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return ErrDuplicateURL
		case "23503":
			return ErrUnknownSection
		}
	}
	return err
}
