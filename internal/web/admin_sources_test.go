package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func adminCookie(t *testing.T, srv *Server, h http.Handler) *http.Cookie {
	t.Helper()
	if _, err := srv.Pool.Exec(context.Background(), "UPDATE users SET role = 'admin'"); err != nil {
		t.Fatal(err)
	}
	return login(t, h)
}

func TestAdminRequiresAdmin(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	member := login(t, h)
	if rec := do(t, h, request{path: "/admin/sources", cookie: member}); rec.Code != http.StatusForbidden {
		t.Errorf("member on /admin/sources: status = %d, want 403", rec.Code)
	}
	if rec := get(t, h, "/admin/sources"); rec.Code != http.StatusSeeOther {
		t.Errorf("signed out on /admin/sources: status = %d, want redirect", rec.Code)
	}
}

func TestAdminSourcesCRUD(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	admin := adminCookie(t, srv, h)
	ctx := context.Background()

	if body := do(t, h, request{path: "/", cookie: admin}).Body.String(); !strings.Contains(body, `href="/admin"`) {
		t.Error("admin doesn't see the admin link")
	}

	rec := do(t, h, request{method: http.MethodPost, path: "/admin/sources", cookie: admin,
		form: url.Values{"name": {""}, "url": {"ftp://x"}, "section_id": {"1"}}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "URL must be an absolute") {
		t.Fatalf("invalid create: status = %d", rec.Code)
	}

	form := url.Values{"name": {"Krebs"}, "url": {"https://krebsonsecurity.com/feed/"}, "section_id": {"1"}, "enabled": {"on"}}
	rec = do(t, h, request{method: http.MethodPost, path: "/admin/sources", cookie: admin, form: form})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status = %d, body = %s", rec.Code, rec.Body)
	}
	var id int64
	if err := srv.Pool.QueryRow(ctx, "SELECT id FROM sources WHERE name = 'Krebs'").Scan(&id); err != nil {
		t.Fatal(err)
	}

	rec = do(t, h, request{method: http.MethodPost, path: "/admin/sources", cookie: admin, form: form})
	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("duplicate URL: status = %d", rec.Code)
	}

	if body := do(t, h, request{path: "/admin/sources", cookie: admin}).Body.String(); !strings.Contains(body, "Krebs") {
		t.Error("list doesn't show the new source")
	}
	if rec := do(t, h, request{path: fmt.Sprintf("/admin/sources/%d", id), cookie: admin}); rec.Code != http.StatusOK {
		t.Errorf("edit page: status = %d", rec.Code)
	}

	rec = do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/admin/sources/%d/enabled", id), cookie: admin,
		form: url.Values{"enabled": {"false"}}})
	var enabled bool
	if err := srv.Pool.QueryRow(ctx, "SELECT enabled FROM sources WHERE id = $1", id).Scan(&enabled); err != nil || enabled {
		t.Errorf("disable: status = %d, enabled = %v, err = %v", rec.Code, enabled, err)
	}

	rec = do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/admin/sources/%d/delete", id), cookie: admin,
		header: map[string]string{"HX-Request": "true"}})
	if rec.Header().Get("HX-Redirect") != "/admin/sources" {
		t.Errorf("delete via htmx: status = %d, HX-Redirect = %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}

	var actions int
	if err := srv.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action LIKE 'source.%'").Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != 3 {
		t.Errorf("%d audited source actions, want 3 (created, disabled, deleted)", actions)
	}
}

func TestAdminSourceTest(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	admin := adminCookie(t, srv, h)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<rss version="2.0"><channel><title>T</title>
			<item><title>First &lt;story&gt;</title><link>https://example.com/1</link></item></channel></rss>`)
	}))
	defer feed.Close()

	rec := do(t, h, request{method: http.MethodPost, path: "/admin/sources/test", cookie: admin,
		form: url.Values{"url": {feed.URL}}, header: map[string]string{"HX-Request": "true"}})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "The feed has 1 items.") || !strings.Contains(body, "First &lt;story&gt;") {
		t.Errorf("preview: status = %d, body = %s", rec.Code, body)
	}
	if strings.Contains(body, "<html") {
		t.Error("preview returned a full page instead of a fragment")
	}
}
