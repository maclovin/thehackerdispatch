package publish

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/fetch"
	"github.com/maclovin/thehackerdispatch/internal/llm"
	"github.com/maclovin/thehackerdispatch/internal/prompts"
	"github.com/maclovin/thehackerdispatch/internal/secretbox"
	"github.com/maclovin/thehackerdispatch/internal/settings"
	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type writer struct {
	broken           bool
	lastInput        string
	lastInstructions string
}

func (w *writer) Generate(_ context.Context, _ string, req llm.Request) (llm.Response, error) {
	w.lastInput = req.Input
	w.lastInstructions = req.Instructions
	if w.broken {
		return llm.Response{Outcome: llm.OutcomeError}, errors.New("down")
	}
	return llm.Response{Outcome: llm.OutcomeOK, JSON: []byte(`{"title": "Rewritten headline.", "summary": "Two **short** paragraphs.",
		"why_it_matters": "Patch today.", "tldr": "One line."}`)}, nil
}

func setup(t *testing.T, model *writer, content settings.Content) (*Publisher, *pgxpool.Pool) {
	pool := storetest.New(t)
	ctx := context.Background()
	box, _ := secretbox.New(bytes.Repeat([]byte{1}, 32))
	llmStore := llm.NewStore(pool, box, discard, func(context.Context, string, string, string) (llm.Provider, error) {
		return model, nil
	})
	actor := audit.Actor{Label: "test"}
	pid, _ := llmStore.CreateProvider(ctx, llm.ProviderInput{Name: "fake", Type: llm.TypeOpenAI, APIKey: "k"}, actor)
	mid, err := llmStore.CreateModel(ctx, llm.ModelInput{ProviderID: pid, ModelID: "w", SupportsJSONSchema: true}, actor)
	if err != nil {
		t.Fatal(err)
	}
	st := settings.New(pool)
	if err := st.SetLLMStages(ctx, settings.LLMStages{Curation: settings.LLMStage{ModelIDs: []int64{mid}}}, actor); err != nil {
		t.Fatal(err)
	}
	if err := st.SetContent(ctx, content, actor); err != nil {
		t.Fatal(err)
	}
	storetest.MustExec(t, pool, `INSERT INTO sources (name, url, section_id) VALUES ('Feed', 'https://example.com/feed', 1)`)
	client := fetch.New(fetch.Options{AllowPrivate: true})
	return New(pool, llmStore, st, prompts.New(pool), client, discard), pool
}

func addStory(t *testing.T, pool *pgxpool.Pool, title, url string, score int, priority string) int64 {
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO stories (title, section_id, score, priority) VALUES ($1, 1, $2, $3) RETURNING id`,
		title, score, priority).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	storetest.MustExec(t, pool, `INSERT INTO story_links (story_id, url, title, source_id, is_primary) VALUES ($1, $2, $3, 1, true)`,
		id, url, title)
	storetest.MustExec(t, pool, `INSERT INTO raw_items (source_id, guid_hash, url, title, snippet, status, story_id)
		VALUES (1, $1, $2, $3, 'The feed snippet.', 'curated', $4)`, title, url, title, id)
	return id
}

func TestRunOnceWritesAndPublishes(t *testing.T) {
	articles := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head><title>T</title></head><body><nav>menu</nav><article><h1>Big news</h1>
			<p>A critical flaw in ExampleOS 1.2 is exploited in the wild. Researchers published details today and the
			vendor shipped a fix in version 1.3. Administrators should upgrade immediately.</p>
			<p>More details about the attack chain and indicators of compromise are available.</p></article></body></html>`)
	}))
	defer articles.Close()

	model := &writer{}
	pub, pool := setup(t, model, settings.Content{TTLDays: 7, DailyCap: 10, MinScore: 40})
	id := addStory(t, pool, "Original title", articles.URL+"/post", 80, "normal")
	actor := audit.Actor{Label: "admin"}
	prompts.New(pool).Save(context.Background(), prompts.Global, "Write for busy people.", actor)
	prompts.New(pool).Save(context.Background(), 1, "Always name the CVE.", actor)

	if err := pub.Publish(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var (
		status, title, summary, model2 string
		expires                        *time.Time
	)
	err := pool.QueryRow(context.Background(), "SELECT status, title, summary_md, model, expires_at FROM stories WHERE id = $1", id).
		Scan(&status, &title, &summary, &model2, &expires)
	if err != nil {
		t.Fatal(err)
	}
	if status != "published" || title != "Rewritten headline" || summary != "Two **short** paragraphs." || model2 != "fake / w" {
		t.Errorf("story = %s %q %q %q", status, title, summary, model2)
	}
	if expires == nil || time.Until(*expires) < 6*24*time.Hour {
		t.Errorf("expires_at = %v, want about 7 days from now", expires)
	}
	if !strings.Contains(model.lastInput, "exploited in the wild") || strings.Contains(model.lastInput, "menu") {
		t.Errorf("the model should get the article's main text, without navigation:\n%s", model.lastInput)
	}
	want := Contract + "\n\nGLOBAL PROMPT (written by the board's admins):\nWrite for busy people." +
		"\n\nSECTION PROMPT for Security (written by the board's admins):\nAlways name the CVE."
	if model.lastInstructions != want {
		t.Errorf("instructions:\n%s", model.lastInstructions)
	}
}

func TestPublishQueuesTheSlackAlert(t *testing.T) {
	pub, pool := setup(t, &writer{}, settings.Content{TTLDays: 0})
	ctx := context.Background()
	storetest.MustExec(t, pool, "INSERT INTO notification_channels (name, slack_channel) VALUES ('Security news', '#sec-news')")
	storetest.MustExec(t, pool, "UPDATE sections SET notification_channel_id = 1 WHERE slug = 'security'")
	alerted := addStory(t, pool, "In security", "https://example.invalid/1", 90, "normal")
	quiet := addStory(t, pool, "In tech", "https://example.invalid/2", 90, "normal")
	storetest.MustExec(t, pool, "UPDATE stories SET section_id = (SELECT id FROM sections WHERE slug = 'tech') WHERE id = $1", quiet)

	for _, id := range []int64{alerted, quiet, alerted} {
		if err := pub.Publish(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	var (
		story   int64
		channel string
		n       int
	)
	if err := pool.QueryRow(ctx, "SELECT story_id, slack_channel, count(*) OVER () FROM slack_outbox").Scan(&story, &channel, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || story != alerted || channel != "#sec-news" {
		t.Errorf("outbox: %d messages, story %d, channel %q", n, story, channel)
	}
	var forever bool
	if err := pool.QueryRow(ctx, "SELECT expires_at IS NULL FROM stories WHERE id = $1", alerted).Scan(&forever); err != nil || !forever {
		t.Error("TTL 'forever' should leave expires_at empty")
	}
}

func TestFallbackWhenModelFails(t *testing.T) {
	pub, pool := setup(t, &writer{broken: true}, settings.DefaultContent())
	id := addStory(t, pool, "Feed title", "https://example.invalid/x", 90, "normal")
	if err := pub.Publish(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var title, summary, model string
	if err := pool.QueryRow(context.Background(), "SELECT title, summary_md, model FROM stories WHERE id = $1", id).
		Scan(&title, &summary, &model); err != nil {
		t.Fatal(err)
	}
	if title != "Feed title" || summary != "The feed snippet." || model != "" {
		t.Errorf("fallback post = %q %q %q", title, summary, model)
	}
}
