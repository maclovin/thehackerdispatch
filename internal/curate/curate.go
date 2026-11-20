package curate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/llm"
	"github.com/maclovin/thehackerdispatch/internal/notify"
	"github.com/maclovin/thehackerdispatch/internal/prompts"
	"github.com/maclovin/thehackerdispatch/internal/settings"
)

const (
	batchSize         = 40
	maxAttempts       = 3
	recentDays        = 14
	knownStoriesLimit = 300
	similarPerItem    = 3
	maxLinksPerStory  = 5
	maxEntities       = 20
	duplicateBoost    = 3
)

const Stage = "curation"

type Curator struct {
	pool     *pgxpool.Pool
	llm      *llm.Store
	settings *settings.Store
	prompts  *prompts.Store
	logger   *slog.Logger
}

func New(pool *pgxpool.Pool, llmStore *llm.Store, settings *settings.Store, prompts *prompts.Store, logger *slog.Logger) *Curator {
	return &Curator{pool: pool, llm: llmStore, settings: settings, prompts: prompts, logger: logger}
}

type Stats struct {
	Items, Discarded, Duplicates, Updates, NewStories, Retried int
}

func (c *Curator) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	stages, err := c.settings.LLMStages(ctx)
	if err != nil {
		return stats, err
	}
	if len(stages.Curation.ModelIDs) == 0 {
		c.logger.Info("curation paused: no model configured")
		return stats, nil
	}
	sectionList, err := c.sections(ctx)
	if err != nil {
		return stats, err
	}
	sections := make(map[string]int64, len(sectionList))
	slugs := make([]string, 0, len(sectionList))
	for _, sec := range sectionList {
		sections[sec.Slug] = sec.ID
		slugs = append(slugs, sec.Slug)
	}

	// Not nil: a nil slice is NULL in SQL, and "id <> ALL(NULL)" would exclude every item.
	tried := []int64{}
	for {
		items, err := c.pending(ctx, tried)
		if err != nil || len(items) == 0 {
			return stats, err
		}
		for _, it := range items {
			tried = append(tried, it.ID)
		}
		known, err := c.known(ctx, items)
		if err != nil {
			return stats, err
		}
		section := sectionInfo{ID: items[0].SectionID, Slug: items[0].Section, Name: items[0].Section}
		for _, sec := range sectionList {
			if sec.ID == section.ID {
				section = sec
			}
		}
		global, sectionPrompt, err := c.prompts.For(ctx, section.ID)
		if err != nil {
			return stats, err
		}
		req := llm.Request{
			Instructions: instructions(sectionList, global, section, sectionPrompt),
			Context:      knownContext(known),
			Input:        itemsInput(items),
			Schema:       schema(slugs),
			SchemaName:   "curation_decisions",
			MaxTokens:    16000,
		}
		resp, model, err := c.llm.Generate(ctx, Stage, stages.Curation.ModelIDs, llm.Effort(stages.Curation.Effort), req)
		if err != nil {
			if retryErr := c.countAttempt(ctx, items, err); retryErr != nil {
				return stats, retryErr
			}
			if errors.Is(err, llm.ErrRefused) {
				stats.Retried += len(items)
				continue
			}
			return stats, err
		}
		var answer struct {
			Decisions []decision `json:"decisions"`
		}
		if err := json.Unmarshal(resp.JSON, &answer); err != nil {
			if retryErr := c.countAttempt(ctx, items, err); retryErr != nil {
				return stats, retryErr
			}
			return stats, fmt.Errorf("decode decisions: %w", err)
		}
		batch, err := c.apply(ctx, items, answer.Decisions, known, sections, model.Label())
		if err != nil {
			return stats, err
		}
		stats.Items += batch.Items
		stats.Discarded += batch.Discarded
		stats.Duplicates += batch.Duplicates
		stats.Updates += batch.Updates
		stats.NewStories += batch.NewStories
		stats.Retried += batch.Retried
	}
}

func (c *Curator) sections(ctx context.Context) ([]sectionInfo, error) {
	rows, err := c.pool.Query(ctx, "SELECT id, slug, name FROM sections ORDER BY sort, name")
	if err != nil {
		return nil, fmt.Errorf("list sections: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[sectionInfo])
}

func (c *Curator) pending(ctx context.Context, exclude []int64) ([]pendingItem, error) {
	rows, err := c.pool.Query(ctx, `
		WITH next AS (
			SELECT s.section_id FROM raw_items r JOIN sources s ON s.id = r.source_id
			WHERE r.status = 'pending' AND r.id <> ALL($2)
			ORDER BY r.fetched_at, r.id LIMIT 1
		)
		SELECT r.id, s.name, sec.slug, s.section_id, s.id, r.url, r.title, r.snippet, r.published_at, r.attempts
		FROM raw_items r JOIN sources s ON s.id = r.source_id JOIN sections sec ON sec.id = s.section_id
		WHERE r.status = 'pending' AND r.id <> ALL($2) AND s.section_id = (SELECT section_id FROM next)
		ORDER BY r.fetched_at, r.id
		LIMIT $1`, batchSize, exclude)
	if err != nil {
		return nil, fmt.Errorf("list pending items: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[pendingItem])
}

func (c *Curator) known(ctx context.Context, items []pendingItem) ([]knownStory, error) {
	queries := []string{}
	cves := []string{}
	for _, it := range items {
		if q := searchQuery(it.Title); q != "" {
			queries = append(queries, q)
		}
		for _, cve := range cvePattern.FindAllString(it.Title+" "+it.Snippet, -1) {
			cves = append(cves, strings.ToUpper(cve))
		}
	}
	rows, err := c.pool.Query(ctx, `
		WITH live AS (
			SELECT * FROM stories WHERE expires_at IS NULL OR expires_at > now()
		), recent AS (
			SELECT id FROM live WHERE created_at > now() - make_interval(days => $1)
			ORDER BY created_at DESC LIMIT $2
		), alike AS (
			SELECT m.id FROM unnest($3::text[]) AS q(words)
			CROSS JOIN LATERAL (
				SELECT id FROM live WHERE search @@ to_tsquery('english', q.words)
				ORDER BY ts_rank(search, to_tsquery('english', q.words)) DESC, created_at DESC
				LIMIT $4
			) m
		), same_cve AS (
			SELECT id FROM live WHERE EXISTS (SELECT 1 FROM unnest(entities) e WHERE upper(e) = ANY($5))
		)
		SELECT s.id, sec.slug, s.status, coalesce(s.published_at, s.created_at), s.title, s.tldr, s.entities
		FROM stories s JOIN sections sec ON sec.id = s.section_id
		WHERE s.id IN (SELECT id FROM recent UNION SELECT id FROM alike UNION SELECT id FROM same_cve)
		ORDER BY s.created_at DESC`, recentDays, knownStoriesLimit, queries, similarPerItem, cves)
	if err != nil {
		return nil, fmt.Errorf("list known stories: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[knownStory])
}

var cvePattern = regexp.MustCompile(`(?i)\bCVE-\d{4}-\d{4,7}\b`)

// searchQuery keeps only letters and digits, so to_tsquery can't fail on the input.
func searchQuery(title string) string {
	seen := map[string]bool{}
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) < 3 || seen[w] {
			continue
		}
		seen[w] = true
		words = append(words, w)
	}
	return strings.Join(words, " | ")
}

func (c *Curator) countAttempt(ctx context.Context, items []pendingItem, cause error) error {
	reason := "curation_failed"
	if errors.Is(cause, llm.ErrRefused) {
		reason = "refused"
	}
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	_, err := c.pool.Exec(ctx, `
		UPDATE raw_items SET
			attempts = attempts + 1,
			status = CASE WHEN attempts + 1 >= $2 THEN 'failed' ELSE status END,
			decision = CASE WHEN attempts + 1 >= $2 THEN 'discard' ELSE decision END,
			reason = CASE WHEN attempts + 1 >= $2 THEN $3 ELSE reason END
		WHERE id = ANY($1)`, ids, maxAttempts, reason)
	return err
}

func (c *Curator) apply(ctx context.Context, items []pendingItem, decisions []decision, known []knownStory,
	sections map[string]int64, model string) (Stats, error) {
	byItem := make(map[int64]decision, len(decisions))
	for _, d := range decisions {
		byItem[d.ItemID] = d
	}
	knownStatus := make(map[int64]string, len(known))
	for _, k := range known {
		knownStatus[k.ID] = k.Status
	}

	var stats Stats
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		newStories := map[string]int64{}
		for _, it := range items {
			d, ok := byItem[it.ID]
			if !ok {
				stats.Retried++
				if _, err := tx.Exec(ctx, `
					UPDATE raw_items SET attempts = attempts + 1,
						status = CASE WHEN attempts + 1 >= $2 THEN 'failed' ELSE status END,
						reason = CASE WHEN attempts + 1 >= $2 THEN 'no decision returned' ELSE reason END
					WHERE id = $1`, it.ID, maxAttempts); err != nil {
					return err
				}
				continue
			}
			stats.Items++
			d.normalize(it, sections)

			if d.Decision == "duplicate" || d.Decision == "update" {
				storyID, ok := parseRef(d.StoryRef)
				status, isKnown := knownStatus[storyID]
				if !ok || !isKnown {
					d.Decision, d.StoryRef = "new", "orphan-"+strconv.FormatInt(it.ID, 10)
				} else {
					if err := c.attach(ctx, tx, storyID, status, it, d); err != nil {
						return err
					}
					if d.Decision == "duplicate" {
						stats.Duplicates++
					} else {
						stats.Updates++
					}
					if err := markCurated(ctx, tx, it.ID, d, storyID); err != nil {
						return err
					}
					continue
				}
			}

			switch d.Decision {
			case "discard":
				stats.Discarded++
				if err := markCurated(ctx, tx, it.ID, d, 0); err != nil {
					return err
				}
			default:
				storyID, exists := newStories[d.StoryRef]
				if exists {
					if err := addLink(ctx, tx, storyID, it, false); err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, `
						UPDATE stories SET score = greatest(score, $2),
							priority = CASE WHEN $3 = 'critical' THEN 'critical' ELSE priority END,
							entities = ARRAY(SELECT DISTINCT unnest(entities || $4::text[]) LIMIT 20)
						WHERE id = $1`, storyID, d.Score, d.Priority, d.Entities); err != nil {
						return err
					}
				} else {
					err := tx.QueryRow(ctx, `
						INSERT INTO stories (status, title, tldr, section_id, score, priority, entities, model)
						VALUES ('queued', $1, $2, $3, $4, $5, $6, $7) RETURNING id`,
						it.Title, truncate(it.Snippet, 300), sections[d.Section], d.Score, d.Priority, d.Entities, model).Scan(&storyID)
					if err != nil {
						return fmt.Errorf("create story: %w", err)
					}
					if err := addLink(ctx, tx, storyID, it, true); err != nil {
						return err
					}
					if d.Priority != "critical" {
						if _, err := tx.Exec(ctx, "INSERT INTO pacing_arrivals (score) VALUES ($1)", d.Score); err != nil {
							return err
						}
					}
					newStories[d.StoryRef] = storyID
					stats.NewStories++
				}
				if err := markCurated(ctx, tx, it.ID, d, storyID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return stats, err
}

func (c *Curator) attach(ctx context.Context, tx pgx.Tx, storyID int64, status string, it pendingItem, d decision) error {
	if err := addLink(ctx, tx, storyID, it, false); err != nil {
		return err
	}
	switch {
	case d.Decision == "duplicate" && status == "queued":
		_, err := tx.Exec(ctx, "UPDATE stories SET score = least(100, score + $2) WHERE id = $1", storyID, duplicateBoost)
		return err
	case d.Decision == "update" && status == "published" && d.UpdateNote != "":
		var updateID int64
		err := tx.QueryRow(ctx, `
			INSERT INTO story_updates (story_id, note, url)
			SELECT $1, $2, $3
			WHERE NOT EXISTS (SELECT 1 FROM story_updates WHERE story_id = $1 AND created_at > now() - interval '6 hours')
			RETURNING id`,
			storyID, truncate(d.UpdateNote, 500), it.URL).Scan(&updateID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return notify.EnqueueReply(ctx, tx, storyID, updateID)
	}
	return nil
}

func addLink(ctx context.Context, tx pgx.Tx, storyID int64, it pendingItem, primary bool) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO story_links (story_id, url, title, source_id, is_primary)
		SELECT $1, $2, $3, $4, $5
		WHERE (SELECT count(*) FROM story_links WHERE story_id = $1) < $6
		ON CONFLICT (story_id, url) DO NOTHING`,
		storyID, it.URL, it.Title, it.SourceID, primary, maxLinksPerStory)
	return err
}

func markCurated(ctx context.Context, tx pgx.Tx, itemID int64, d decision, storyID int64) error {
	var story *int64
	if storyID != 0 {
		story = &storyID
	}
	_, err := tx.Exec(ctx, `
		UPDATE raw_items SET status = 'curated', decision = $2, reason = $3, story_id = $4 WHERE id = $1`,
		itemID, d.Decision, truncate(d.Reason, 300), story)
	return err
}

func (d *decision) normalize(it pendingItem, sections map[string]int64) {
	if _, ok := sections[d.Section]; !ok {
		d.Section = it.Section
	}
	d.Score = max(0, min(100, d.Score))
	if d.Priority != "critical" {
		d.Priority = "normal"
	}
	if d.Decision != "discard" && d.Decision != "duplicate" && d.Decision != "update" {
		d.Decision = "new"
	}
	if d.Decision == "new" && d.StoryRef == "" {
		d.StoryRef = "item-" + strconv.FormatInt(it.ID, 10)
	}
	seen := map[string]bool{}
	var entities []string
	for _, e := range d.Entities {
		e = strings.TrimSpace(e)
		if e == "" || len(e) > 100 || seen[strings.ToLower(e)] || len(entities) == maxEntities {
			continue
		}
		seen[strings.ToLower(e)] = true
		entities = append(entities, e)
	}
	d.Entities = entities
	if d.Entities == nil {
		d.Entities = []string{}
	}
}

func parseRef(ref string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(ref), "S"), 10, 64)
	return id, err == nil && id > 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // don't cut a UTF-8 character in half
		n--
	}
	return s[:n]
}
