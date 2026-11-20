package sources

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

var (
	ErrSectionNotFound  = errors.New("section not found")
	ErrDuplicateSection = errors.New("a section with this slug or name already exists")
	ErrSectionInUse     = errors.New("this section still has sources or stories: move or delete them first")
	ErrLastSection      = errors.New("the board needs at least one section")
	ErrUnknownChannel   = errors.New("that notification channel no longer exists")
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

type SectionInput struct {
	Slug string
	Name string
}

func (in *SectionInput) Validate() error {
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	in.Name = strings.TrimSpace(in.Name)
	var errs []error
	if len(in.Slug) > 30 || !slugPattern.MatchString(in.Slug) {
		errs = append(errs, errors.New("slug must have 1 to 30 lowercase letters, digits or dashes, like threat-intel"))
	}
	if in.Name == "" || len([]rune(in.Name)) > 40 {
		errs = append(errs, errors.New("name must have 1 to 40 characters"))
	}
	return errors.Join(errs...)
}

type SectionSummary struct {
	Section
	Sources       int
	Stories       int
	PromptVersion *int
	ChannelID     *int64
	ChannelName   string
}

func (s SectionSummary) HasPrompt() bool { return s.PromptVersion != nil }

func (s *Store) SectionSummaries(ctx context.Context) ([]SectionSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sec.id, sec.slug, sec.name, sec.sort,
		       (SELECT count(*) FROM sources WHERE section_id = sec.id),
		       (SELECT count(*) FROM stories WHERE section_id = sec.id),
		       (SELECT version FROM prompts WHERE section_id = sec.id AND body <> ''
		        AND version = (SELECT max(version) FROM prompts WHERE section_id = sec.id)),
		       sec.notification_channel_id, coalesce(c.name, '')
		FROM sections sec LEFT JOIN notification_channels c ON c.id = sec.notification_channel_id
		ORDER BY sec.sort, sec.name`)
	if err != nil {
		return nil, fmt.Errorf("list sections: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (SectionSummary, error) {
		var s SectionSummary
		err := row.Scan(&s.ID, &s.Slug, &s.Name, &s.Sort, &s.Sources, &s.Stories, &s.PromptVersion,
			&s.ChannelID, &s.ChannelName)
		return s, err
	})
}

func (s *Store) Section(ctx context.Context, id int64) (Section, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, slug, name, sort FROM sections WHERE id = $1", id)
	if err != nil {
		return Section{}, fmt.Errorf("get section: %w", err)
	}
	sec, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Section])
	if errors.Is(err, pgx.ErrNoRows) {
		return Section{}, ErrSectionNotFound
	}
	return sec, err
}

func (s *Store) CreateSection(ctx context.Context, in SectionInput, actor audit.Actor) (int64, error) {
	if err := in.Validate(); err != nil {
		return 0, err
	}
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO sections (slug, name, sort)
			SELECT $1, $2, coalesce(max(sort), 0) + 1 FROM sections
			RETURNING id`, in.Slug, in.Name).Scan(&id)
		if err != nil {
			return mapSectionError(err)
		}
		return audit.Record(ctx, tx, actor.Entry("section.created", sectionTarget(id), map[string]any{"slug": in.Slug, "name": in.Name}))
	})
	return id, err
}

func (s *Store) RenameSection(ctx context.Context, id int64, name string, actor audit.Actor) error {
	in := SectionInput{Slug: "x", Name: name}
	if err := in.Validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE sections SET name = $2 WHERE id = $1", id, in.Name)
		if err != nil {
			return mapSectionError(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrSectionNotFound
		}
		return audit.Record(ctx, tx, actor.Entry("section.renamed", sectionTarget(id), map[string]any{"name": in.Name}))
	})
}

func (s *Store) SetSectionChannel(ctx context.Context, id, channelID int64, actor audit.Actor) error {
	var channel *int64
	if channelID != 0 {
		channel = &channelID
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE sections SET notification_channel_id = $2 WHERE id = $1", id, channel)
		if err != nil {
			return mapSectionError(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrSectionNotFound
		}
		return audit.Record(ctx, tx, actor.Entry("section.channel_set", sectionTarget(id), map[string]any{"channel_id": channel}))
	})
}

func (s *Store) MoveSection(ctx context.Context, id int64, delta int) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT id FROM sections ORDER BY sort, name FOR UPDATE")
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		i := -1
		for k, v := range ids {
			if v == id {
				i = k
			}
		}
		if i < 0 {
			return ErrSectionNotFound
		}
		j := i + 1
		if delta < 0 {
			j = i - 1
		}
		if j >= 0 && j < len(ids) {
			ids[i], ids[j] = ids[j], ids[i]
		}
		_, err = tx.Exec(ctx, `
			UPDATE sections SET sort = o.n FROM unnest($1::bigint[]) WITH ORDINALITY AS o(id, n)
			WHERE sections.id = o.id`, ids)
		return err
	})
}

func (s *Store) DeleteSection(ctx context.Context, id int64, actor audit.Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var total int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM sections").Scan(&total); err != nil {
			return err
		}
		if total <= 1 {
			return ErrLastSection
		}
		var slug string
		err := tx.QueryRow(ctx, "DELETE FROM sections WHERE id = $1 RETURNING slug", id).Scan(&slug)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSectionNotFound
		}
		if err != nil {
			return mapSectionError(err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET preferred_sections = array_remove(preferred_sections, $1)
			WHERE $1 = ANY(preferred_sections)`, slug); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Entry("section.deleted", sectionTarget(id), map[string]any{"slug": slug}))
	})
}

func sectionTarget(id int64) string { return fmt.Sprintf("section:%d", id) }

func mapSectionError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return ErrDuplicateSection
		case "23503":
			if pgErr.ConstraintName == "sections_notification_channel_id_fkey" {
				return ErrUnknownChannel
			}
			return ErrSectionInUse
		}
	}
	return err
}
