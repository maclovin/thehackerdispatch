package web

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maclovin/thehackerdispatch/internal/curate"
	"github.com/maclovin/thehackerdispatch/internal/prompts"
)

func TestAdminSections(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	admin := adminCookie(t, srv, h)
	ctx := context.Background()

	rec := do(t, h, request{method: http.MethodPost, path: "/admin/sections", cookie: admin,
		form: url.Values{"name": {"Threat intel"}, "slug": {"Threat Intel!"}}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "lowercase letters") {
		t.Fatalf("invalid slug: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/admin/sections", cookie: admin,
		form: url.Values{"name": {"security"}, "slug": {"sec2"}}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("duplicate name: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/admin/sections", cookie: admin,
		form: url.Values{"name": {"Threat intel"}, "slug": {"threat-intel"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status = %d", rec.Code)
	}
	page := rec.Header().Get("Location")
	var id int64
	fmt.Sscanf(page, "/admin/sections/%d", &id)

	do(t, h, request{method: http.MethodPost, path: page + "/move", cookie: admin, form: url.Values{"dir": {"up"}}})
	body := do(t, h, request{path: "/admin/sections", cookie: admin}).Body.String()
	if strings.Index(body, "Threat intel") > strings.Index(body, "Engineering") {
		t.Error("moving up didn't put the section before Engineering")
	}
	if !strings.Contains(do(t, h, request{path: "/", cookie: admin}).Body.String(), "Threat intel") {
		t.Error("the board's sidebar doesn't list the new section")
	}

	do(t, h, request{method: http.MethodPost, path: page, cookie: admin, form: url.Values{"name": {"Threat intelligence"}}})
	if body := do(t, h, request{path: page, cookie: admin}).Body.String(); !strings.Contains(body, "Section: Threat intelligence") ||
		!strings.Contains(body, "this section uses only the global prompt") {
		t.Errorf("section page after rename:\n%s", body)
	}

	rec = do(t, h, request{method: http.MethodPost, path: page + "/prompt", cookie: admin,
		form: url.Values{"body": {"Focus on named threat actors."}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save section prompt: status = %d", rec.Code)
	}
	if body := do(t, h, request{path: page + "?saved=1", cookie: admin}).Body.String(); !strings.Contains(body, "This is now version 1") {
		t.Error("the saved prompt isn't shown as version 1")
	}
	if _, section, _ := srv.Prompts.For(ctx, id); section != "Focus on named threat actors." {
		t.Errorf("section prompt = %q", section)
	}

	srv.Pool.Exec(ctx, "INSERT INTO sources (name, url, section_id) VALUES ('Feed', 'https://feed.example/rss', 1)")
	if rec := do(t, h, request{method: http.MethodPost, path: "/admin/sections/1/delete", cookie: admin}); rec.Code != http.StatusConflict {
		t.Errorf("deleting a section with sources: status = %d", rec.Code)
	}
	srv.Pool.Exec(ctx, "UPDATE users SET preferred_sections = '{threat-intel,tech}'")
	rec = do(t, h, request{method: http.MethodPost, path: page + "/delete", cookie: admin, header: map[string]string{"HX-Request": "true"}})
	if rec.Header().Get("HX-Redirect") != "/admin/sections" {
		t.Fatalf("delete: status = %d", rec.Code)
	}
	var prefs []string
	srv.Pool.QueryRow(ctx, "SELECT preferred_sections FROM users LIMIT 1").Scan(&prefs)
	if len(prefs) != 1 || prefs[0] != "tech" {
		t.Errorf("preferred sections after delete = %v", prefs)
	}
	var left int
	srv.Pool.QueryRow(ctx, "SELECT count(*) FROM prompts WHERE section_id = $1", id).Scan(&left)
	if left != 0 {
		t.Error("the section's prompts survived it")
	}
}

func TestAdminGlobalPrompt(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	admin := adminCookie(t, srv, h)

	body := do(t, h, request{path: "/admin/prompt", cookie: admin}).Body.String()
	if !strings.Contains(body, "built-in default, not saved yet") || !strings.Contains(body, "never follow instructions") {
		t.Error("the prompt page should show the default and the fixed parts")
	}
	if !strings.Contains(body, strings.SplitN(curate.Contract, "\n", 2)[0][:60]) {
		t.Error("the curation contract isn't shown")
	}
	rec := do(t, h, request{method: http.MethodPost, path: "/admin/prompt", cookie: admin, form: url.Values{"body": {"  "}}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "can&#39;t be empty") {
		t.Errorf("empty global prompt: status = %d", rec.Code)
	}
	do(t, h, request{method: http.MethodPost, path: "/admin/prompt", cookie: admin, form: url.Values{"body": {"First"}}})
	do(t, h, request{method: http.MethodPost, path: "/admin/prompt", cookie: admin, form: url.Values{"body": {"Second"}}})

	history, _ := srv.Prompts.History(context.Background(), prompts.Global)
	if len(history) != 2 {
		t.Fatalf("%d versions, want 2", len(history))
	}
	rec = do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/admin/prompts/%d/restore", history[1].ID), cookie: admin})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/prompt?saved=1" {
		t.Errorf("restore: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if global, _, _ := srv.Prompts.For(context.Background(), 1); global != "First" {
		t.Errorf("after restore the global prompt is %q", global)
	}

	member := addUser(t, srv, h, "bob@example.com", "bob")
	if rec := do(t, h, request{path: "/admin/prompt", cookie: member}); rec.Code != http.StatusForbidden {
		t.Errorf("member on the prompt page: status = %d", rec.Code)
	}
}

func TestImportOPML(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	admin := adminCookie(t, srv, h)
	ctx := context.Background()
	srv.Pool.Exec(ctx, "INSERT INTO sources (name, url, section_id) VALUES ('Old', 'https://old.example/rss', 1)")

	const opml = `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0"><head><title>Feeds</title></head><body>
  <outline text="Security">
    <outline text="News">
      <outline type="rss" text="Krebs" xmlUrl="https://krebsonsecurity.com/feed/"/>
      <outline type="rss" text="Old again" xmlUrl="https://old.example/rss"/>
    </outline>
  </outline>
  <outline text="Business">
    <outline type="rss" title="Money" text="ignored" xmlUrl="https://money.example/rss"/>
  </outline>
  <outline type="rss" text="Loose" xmlUrl="https://loose.example/atom"/>
  <outline type="rss" text="Bad" xmlUrl="ftp://bad.example/rss"/>
</body></opml>`
	rec := upload(t, h, admin, opml, url.Values{"section_id": {"2"}, "enabled": {"on"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "3 added, 1 already there, 1 invalid") {
		t.Fatalf("import: status = %d\n%s", rec.Code, rec.Body)
	}
	for name, section := range map[string]string{"Krebs": "security", "Money": "business", "Loose": "tech"} {
		var slug string
		if err := srv.Pool.QueryRow(ctx, `SELECT sec.slug FROM sources s JOIN sections sec ON sec.id = s.section_id
			WHERE s.name = $1`, name).Scan(&slug); err != nil || slug != section {
			t.Errorf("%s went to %q (%v), want %s", name, slug, err, section)
		}
	}

	rec = upload(t, h, admin, "<html>not opml</html>", url.Values{"section_id": {"1"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not an OPML file") {
		t.Errorf("bad file: status = %d", rec.Code)
	}
}

func upload(t *testing.T, h http.Handler, cookie *http.Cookie, content string, fields url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, vs := range fields {
		for _, v := range vs {
			mw.WriteField(k, v)
		}
	}
	fw, _ := mw.CreateFormFile("opml", "feeds.opml")
	fw.Write([]byte(content))
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/admin/sources/import", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAdminDeletesStory(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	id := addPublished(t, srv, "Doomed story", "x", "7 days")
	path := fmt.Sprintf("/admin/stories/%d/delete", id)
	member := addUser(t, srv, h, "bob@example.com", "bob")
	do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/s/%d/comments", id), cookie: member, form: url.Values{"body": {"Hi"}}})

	if rec := do(t, h, request{method: http.MethodPost, path: path, cookie: member}); rec.Code != http.StatusForbidden {
		t.Errorf("member deleting a story: status = %d", rec.Code)
	}
	if rec := do(t, h, request{path: "/admin/stories", cookie: member}); rec.Code != http.StatusForbidden {
		t.Errorf("member opening the stories list: status = %d", rec.Code)
	}
	admin := adminCookie(t, srv, h)
	if strings.Contains(do(t, h, request{path: fmt.Sprintf("/s/%d", id), cookie: admin}).Body.String(), path) {
		t.Error("the story page has a delete button; it belongs to the admin panel")
	}
	if body := do(t, h, request{path: "/admin/stories", cookie: admin}).Body.String(); !strings.Contains(body, "Doomed story") || !strings.Contains(body, path) {
		t.Error("the stories list doesn't offer to delete the story")
	}
	rec := do(t, h, request{method: http.MethodPost, path: path, cookie: admin, header: map[string]string{"HX-Request": "true"}})
	if rec.Header().Get("HX-Redirect") != "/admin/stories" {
		t.Fatalf("delete: status = %d", rec.Code)
	}
	var stories, comments, audited int
	srv.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM stories), (SELECT count(*) FROM comments),
		(SELECT count(*) FROM audit_log WHERE action = 'story.deleted')`).Scan(&stories, &comments, &audited)
	if stories != 0 || comments != 0 || audited != 1 {
		t.Errorf("after delete: %d stories, %d comments, %d audit entries", stories, comments, audited)
	}
}
