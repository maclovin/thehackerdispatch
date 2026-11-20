package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func addPublished(t *testing.T, srv *Server, title, summary, expires string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := srv.Pool.Exec(ctx, `INSERT INTO sources (name, url, section_id) VALUES ('Feed', 'https://feed.example/rss', 1)
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var id int64
	err := srv.Pool.QueryRow(ctx, `
		INSERT INTO stories (status, title, summary_md, why_it_matters, section_id, priority, published_at, expires_at, model)
		VALUES ('published', $1, $2, 'Because.', 1, 'critical', now(), now() + $3::interval, 'fake / m') RETURNING id`,
		title, summary, expires).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Pool.Exec(ctx, `INSERT INTO story_links (story_id, url, title, source_id, is_primary)
		VALUES ($1, 'https://www.example.com/a', 'Original', (SELECT id FROM sources LIMIT 1), true)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBoardAndStory(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	cookie := login(t, h)

	if body := do(t, h, request{path: "/", cookie: cookie}).Body.String(); !strings.Contains(body, "No stories yet") {
		t.Error("empty board should say so")
	}

	id := addPublished(t, srv, "Flaw in <svg> parser", "Some **bold** text.\n\n<script>alert(1)</script>", "7 days")
	addPublished(t, srv, "Expired story", "x", "-1 hour")

	body := do(t, h, request{path: "/", cookie: cookie}).Body.String()
	for _, want := range []string{"Flaw in &lt;svg&gt; parser", "example.com", "critical", "expires in"} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing %q", want)
		}
	}
	if strings.Contains(body, "Expired story") {
		t.Error("expired story on the board")
	}

	rec := do(t, h, request{path: fmt.Sprintf("/s/%d", id), cookie: cookie})
	body = rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("story page: status = %d", rec.Code)
	}
	if !strings.Contains(body, "<strong>bold</strong>") || !strings.Contains(body, "Why it matters:") {
		t.Errorf("story page doesn't render the post:\n%s", body)
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("model-written HTML was not stripped")
	}

	if rec := do(t, h, request{path: "/s/999999", cookie: cookie}); rec.Code != http.StatusNotFound {
		t.Errorf("missing story: status = %d", rec.Code)
	}
}

func TestAdminLLMAndSettings(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	admin := adminCookie(t, srv, h)
	ctx := context.Background()

	if rec := do(t, h, request{path: "/admin/llm", cookie: admin}); rec.Code != http.StatusOK {
		t.Fatalf("llm page: status = %d", rec.Code)
	}
	rec := do(t, h, request{method: http.MethodPost, path: "/admin/llm/providers", cookie: admin,
		form: url.Values{"name": {"Anthropic"}, "type": {"anthropic"}, "api_key": {"sk-ant-abcd1234"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add provider: status = %d, body = %s", rec.Code, rec.Body)
	}
	body := do(t, h, request{path: "/admin/llm", cookie: admin}).Body.String()
	if !strings.Contains(body, "…1234") || strings.Contains(body, "sk-ant-abcd1234") {
		t.Error("the API key should only show its last 4 characters")
	}

	var pid int64
	if err := srv.Pool.QueryRow(ctx, "SELECT id FROM llm_providers").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/admin/llm/models", cookie: admin,
		form: url.Values{"provider_id": {fmt.Sprint(pid)}, "model_id": {"claude-sonnet-5-5"}, "supports_json_schema": {"on"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add model: status = %d, body = %s", rec.Code, rec.Body)
	}
	var mid int64
	if err := srv.Pool.QueryRow(ctx, "SELECT id FROM llm_models").Scan(&mid); err != nil {
		t.Fatal(err)
	}

	rec = do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/admin/llm/models/%d/test", mid), cookie: admin})
	if !strings.Contains(rec.Body.String(), `ok: answered {&#34;ok&#34;:true}`) {
		t.Errorf("model test: %s", rec.Body)
	}

	rec = do(t, h, request{method: http.MethodPost, path: "/admin/llm/stages", cookie: admin,
		form: url.Values{"curation_model_1": {fmt.Sprint(mid)}, "curation_effort": {"low"}, "writing_effort": {"bogus"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save stages: status = %d", rec.Code)
	}
	stages, err := srv.Settings.LLMStages(ctx)
	if err != nil || len(stages.Curation.ModelIDs) != 1 || stages.Curation.Effort != "low" || stages.Writing.Effort != "" {
		t.Errorf("stages = %+v, err = %v", stages, err)
	}

	id := addPublished(t, srv, "Old story", "x", "30 days")
	if _, err := srv.Pool.Exec(ctx, "UPDATE stories SET published_at = now() - interval '3 days' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	settingsForm := url.Values{"poll_interval_minutes": {"30"}, "ttl_days": {"1"}, "daily_cap": {"5"}, "min_score": {"60"}, "min_gap_minutes": {"45"},
		"allowed_domains": {"example.com, @Example.org"}, "session_lifetime_hours": {"8"}, "max_failed_logins": {"3"},
		"login_wait_minutes": {"10"}, "display_name": {"MyInc News"}, "primary_color": {"#123456"},
		"link_color_light": {"#0000ee"}, "visited_color_light": {"#551a8b"}, "link_color_dark": {"#8ab4f8"},
		"visited_color_dark": {"#c58af9"}}
	bad := url.Values{}
	for k, v := range settingsForm {
		bad[k] = v
	}
	bad.Set("primary_color", "red;}")
	if rec := do(t, h, request{method: http.MethodPost, path: "/admin/settings", cookie: admin, form: bad}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid color accepted: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/admin/settings", cookie: admin, form: settingsForm})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save settings: status = %d", rec.Code)
	}
	var expired bool
	if err := srv.Pool.QueryRow(ctx, "SELECT expires_at < now() FROM stories WHERE id = $1", id).Scan(&expired); err != nil || !expired {
		t.Errorf("story published 3 days ago should be expired with a 1-day TTL (err=%v)", err)
	}
	content, _ := srv.Settings.Content(ctx)
	ingest, _ := srv.Settings.Ingest(ctx)
	if content.DailyCap != 5 || content.MinScore != 60 || content.MinGapMinutes != 45 || ingest.PollIntervalMinutes != 30 {
		t.Errorf("settings not saved: %+v %+v", content, ingest)
	}
	authSettings, _ := srv.Settings.Auth(ctx)
	brand, _ := srv.Settings.Branding(ctx)
	if !slices.Equal(authSettings.AllowedEmailDomains, []string{"example.com", "example.org"}) ||
		authSettings.SessionLifetimeHours != 8 || brand.DisplayName != "MyInc News" || brand.PrimaryColor != "#123456" {
		t.Errorf("auth or branding not saved: %+v %+v", authSettings, brand)
	}
	if body := do(t, h, request{path: "/", cookie: admin}).Body.String(); !strings.Contains(body, "disappears 1 day after") {
		t.Error("the footer doesn't show the TTL")
	}
	if body := do(t, h, request{path: "/admin/settings?saved=1", cookie: admin}).Body.String(); !strings.Contains(body, "Saved.") {
		t.Error("settings page doesn't confirm the save")
	}
	if rec := do(t, h, request{path: "/admin/decisions", cookie: admin}); rec.Code != http.StatusOK {
		t.Errorf("decisions page: status = %d", rec.Code)
	}
}
