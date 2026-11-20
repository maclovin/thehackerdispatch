package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codeberg.org/readeck/go-readability/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/fetch"
	"github.com/maclovin/thehackerdispatch/internal/llm"
	"github.com/maclovin/thehackerdispatch/internal/notify"
	"github.com/maclovin/thehackerdispatch/internal/prompts"
	"github.com/maclovin/thehackerdispatch/internal/settings"
)

const Stage = "writing"

const maxArticleChars = 30000

type Publisher struct {
	pool     *pgxpool.Pool
	llm      *llm.Store
	settings *settings.Store
	prompts  *prompts.Store
	fetch    *fetch.Client
	logger   *slog.Logger
}

func New(pool *pgxpool.Pool, llmStore *llm.Store, settings *settings.Store, prompts *prompts.Store,
	client *fetch.Client, logger *slog.Logger) *Publisher {
	return &Publisher{pool: pool, llm: llmStore, settings: settings, prompts: prompts, fetch: client, logger: logger}
}

func (p *Publisher) Publish(ctx context.Context, id int64) error {
	content, err := p.settings.Content(ctx)
	if err != nil {
		p.logger.Error("load content settings, using defaults", "err", err)
	}
	stages, err := p.settings.LLMStages(ctx)
	if err != nil {
		return err
	}
	writing := stages.Writing
	if len(writing.ModelIDs) == 0 {
		writing = stages.Curation
	}
	return p.publish(ctx, id, writing, content)
}

type link struct {
	URL       string
	Title     string
	Source    string
	UserAgent string
}

type Post struct {
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	WhyItMatters string `json:"why_it_matters"`
	TLDR         string `json:"tldr"`
}

func (p *Publisher) publish(ctx context.Context, id int64, stage settings.LLMStage, content settings.Content) error {
	var (
		title, section string
		sectionID      int64
	)
	err := p.pool.QueryRow(ctx, `
		UPDATE stories s SET status = 'publishing' FROM sections sec
		WHERE s.id = $1 AND s.status = 'queued' AND sec.id = s.section_id
		RETURNING s.title, sec.name, sec.id`, id).Scan(&title, &section, &sectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // published or deleted meanwhile
	}
	if err != nil {
		return err
	}

	rows, err := p.pool.Query(ctx, `
		SELECT l.url, l.title, coalesce(src.name, ''), coalesce(src.user_agent, '')
		FROM story_links l LEFT JOIN sources src ON src.id = l.source_id
		WHERE l.story_id = $1 ORDER BY l.is_primary DESC, l.added_at`, id)
	if err != nil {
		return err
	}
	links, err := pgx.CollectRows(rows, pgx.RowToStructByPos[link])
	if err != nil {
		return err
	}
	rows, err = p.pool.Query(ctx, "SELECT snippet FROM raw_items WHERE story_id = $1 AND snippet <> '' ORDER BY id", id)
	if err != nil {
		return err
	}
	snippets, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}

	article := ""
	if len(links) > 0 {
		article = p.article(ctx, links[0])
	}
	post, model, err := p.write(ctx, stage, section, sectionID, links, snippets, article)
	if err != nil {
		p.logger.Warn("writing failed, publishing the feed's own text", "story", id, "err", err)
		post, model = fallbackPost(title, snippets), ""
	}

	now := time.Now()
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE stories SET status = 'published', title = $2, summary_md = $3, why_it_matters = $4, tldr = $5,
				model = $6, published_at = $7, expires_at = $8
			WHERE id = $1`,
			id, post.Title, post.Summary, post.WhyItMatters, post.TLDR, model, now, content.ExpiresAt(now))
		if err != nil {
			return err
		}
		return notify.EnqueuePost(ctx, tx, id)
	})
	if err != nil {
		return fmt.Errorf("save post: %w", err)
	}
	p.logger.Info("story published", "story", id, "title", post.Title, "model", model)
	return nil
}

func (p *Publisher) write(ctx context.Context, stage settings.LLMStage, section string, sectionID int64,
	links []link, snippets []string, article string) (Post, string, error) {
	global, sectionPrompt, err := p.prompts.For(ctx, sectionID)
	if err != nil {
		return Post{}, "", err
	}
	var in strings.Builder
	fmt.Fprintf(&in, "SECTION: %s\n\nSOURCES:\n", section)
	for _, l := range links {
		fmt.Fprintf(&in, "- %s (%s) %s\n", l.Title, l.Source, l.URL)
	}
	if len(snippets) > 0 {
		in.WriteString("\nFEED SNIPPETS:\n")
		for _, s := range snippets {
			fmt.Fprintf(&in, "- %s\n", s)
		}
	}
	if article != "" {
		fmt.Fprintf(&in, "\nARTICLE TEXT (from %s):\n%s\n", links[0].URL, article)
	}

	resp, model, err := p.llm.Generate(ctx, Stage, stage.ModelIDs, llm.Effort(stage.Effort), llm.Request{
		Instructions: instructions(global, section, sectionPrompt),
		Input:        in.String(),
		Schema:       postSchema,
		SchemaName:   "post",
		MaxTokens:    8000,
	})
	if err != nil {
		return Post{}, "", err
	}
	var post Post
	if err := json.Unmarshal(resp.JSON, &post); err != nil {
		return Post{}, "", fmt.Errorf("decode post: %w", err)
	}
	post.Title = cut(strings.TrimSuffix(strings.TrimSpace(post.Title), "."), 120)
	post.Summary = cut(strings.TrimSpace(post.Summary), 4000)
	post.WhyItMatters = cut(strings.TrimSpace(post.WhyItMatters), 600)
	post.TLDR = cut(strings.TrimSpace(post.TLDR), 300)
	if post.Title == "" || post.Summary == "" {
		return Post{}, "", errors.New("the model returned an empty title or summary")
	}
	return post, model.Label(), nil
}

func (p *Publisher) article(ctx context.Context, l link) string {
	header := http.Header{}
	if l.UserAgent != "" {
		header.Set("User-Agent", l.UserAgent)
	}
	resp, err := p.fetch.Get(ctx, l.URL, header)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "html") {
		return ""
	}
	pageURL, err := url.Parse(resp.URL)
	if err != nil {
		return ""
	}
	doc, err := readability.FromReader(bytes.NewReader(resp.Body), pageURL)
	if err != nil {
		return ""
	}
	var text strings.Builder
	if err := doc.RenderText(&text); err != nil {
		return ""
	}
	return cut(strings.TrimSpace(text.String()), maxArticleChars)
}

func ApplyTTL(ctx context.Context, pool *pgxpool.Pool, content settings.Content) error {
	_, err := pool.Exec(ctx, `
		UPDATE stories SET expires_at = CASE WHEN $1 = 0 THEN NULL ELSE published_at + make_interval(days => $1) END
		WHERE status = 'published'`, content.TTLDays)
	return err
}

func fallbackPost(title string, snippets []string) Post {
	post := Post{Title: title}
	if len(snippets) > 0 {
		post.Summary = snippets[0]
		post.TLDR = cut(snippets[0], 200)
	}
	return post
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return strings.TrimSpace(s[:n]) + "…"
}
