package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/maclovin/thehackerdispatch/internal/auth"
)

func addUser(t *testing.T, srv *Server, h http.Handler, email, nickname string) *http.Cookie {
	t.Helper()
	hash, _ := auth.HashPassword(testPassword)
	if _, err := srv.Pool.Exec(context.Background(), `INSERT INTO users (auth_mode, email, nickname, password_hash)
		VALUES ('local', $1, $2, $3)`, email, nickname, hash); err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, request{method: http.MethodPost, path: "/login", form: url.Values{"login": {email}, "password": {testPassword}}})
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("login %s failed: %d", email, rec.Code)
	return nil
}

func TestCommentsAndVotes(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	alice := login(t, h)
	bob := addUser(t, srv, h, "bob@example.com", "bob")
	id := addPublished(t, srv, "A story", "Summary.", "7 days")
	story := fmt.Sprintf("/s/%d", id)

	rec := do(t, h, request{method: http.MethodPost, path: story + "/comments", cookie: alice, form: url.Values{"body": {"  "}}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "write something first") {
		t.Fatalf("empty comment: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: story + "/comments", cookie: alice,
		form: url.Values{"body": {"Hello **world** <script>x</script>"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("comment: status = %d", rec.Code)
	}
	var commentID int64
	if err := srv.Pool.QueryRow(context.Background(), "SELECT id FROM comments").Scan(&commentID); err != nil {
		t.Fatal(err)
	}

	body := do(t, h, request{path: story, cookie: bob}).Body.String()
	if !strings.Contains(body, "<strong>world</strong>") || strings.Contains(body, "<script>x") {
		t.Errorf("comment not rendered safely:\n%s", body)
	}
	if !strings.Contains(body, "Discussion (1)") || !strings.Contains(body, `aria-label="upvote"`) {
		t.Error("bob should see the comment count and vote arrows")
	}
	if strings.Contains(do(t, h, request{path: story, cookie: alice}).Body.String(), `aria-label="upvote"`) {
		t.Error("alice sees vote arrows on her own comment")
	}

	vote := fmt.Sprintf("/c/%d/vote", commentID)
	rec = do(t, h, request{method: http.MethodPost, path: vote, cookie: bob, form: url.Values{"value": {"1"}},
		header: map[string]string{"HX-Request": "true"}})
	if !strings.Contains(rec.Body.String(), `<span class="points" title="votes">1</span>`) || strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("htmx vote: %s", rec.Body)
	}
	rec = do(t, h, request{method: http.MethodPost, path: vote, cookie: bob, form: url.Values{"value": {"0"}}})
	if rec.Code != http.StatusSeeOther || !strings.HasSuffix(rec.Header().Get("Location"), fmt.Sprintf("#c%d", commentID)) {
		t.Errorf("plain-form vote should redirect back to the comment: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/s/%d/vote", id), cookie: bob}); rec.Code != http.StatusNotFound {
		t.Errorf("stories must not have votes: status = %d", rec.Code)
	}

	if rec := do(t, h, request{path: fmt.Sprintf("/c/%d/reply", commentID), cookie: bob}); rec.Code != http.StatusOK {
		t.Errorf("reply page: status = %d", rec.Code)
	}
	if rec := do(t, h, request{path: fmt.Sprintf("/c/%d/edit", commentID), cookie: bob}); rec.Code != http.StatusForbidden {
		t.Errorf("bob editing alice's comment: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/c/%d/edit", commentID), cookie: alice, form: url.Values{"body": {"Edited"}}})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("alice editing her comment: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/c/%d/delete", commentID), cookie: bob})
	if rec.Code != http.StatusForbidden {
		t.Errorf("bob deleting alice's comment: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/c/%d/delete", commentID), cookie: alice,
		header: map[string]string{"HX-Request": "true"}})
	if rec.Header().Get("HX-Redirect") == "" {
		t.Errorf("deleting via htmx should redirect: %d", rec.Code)
	}
}

func TestFirstAccessNeedsNickname(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	cookie := login(t, h)
	if _, err := srv.Pool.Exec(context.Background(), "UPDATE users SET nickname = NULL"); err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, request{path: "/", cookie: cookie})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account" {
		t.Fatalf("without a nickname: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if body := do(t, h, request{path: "/account", cookie: cookie}).Body.String(); !strings.Contains(body, "Pick a nickname") {
		t.Error("the account page doesn't ask for a nickname")
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/account", cookie: cookie, form: url.Values{"nickname": {"admin"}}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("reserved nickname: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/account", cookie: cookie, form: url.Values{"nickname": {"alice2"}, "theme": {"dark"}}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("set nickname: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if body := do(t, h, request{path: "/", cookie: cookie}).Body.String(); !strings.Contains(body, `data-theme="dark"`) {
		t.Error("theme not applied")
	}
}

func TestAdminCreatesAccountAndPasswordRotation(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	admin := adminCookie(t, srv, h)

	rec := do(t, h, request{method: http.MethodPost, path: "/admin/users", cookie: admin,
		form: url.Values{"email": {"dave@evil.com"}, "auth_mode": {"local"}, "role": {"member"}}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("foreign domain: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/admin/users", cookie: admin,
		form: url.Values{"email": {"dave@example.com"}, "auth_mode": {"local"}, "role": {"member"}}})
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the page with a generated password must not be cached")
	}
	temp := between(rec.Body.String(), "<tr><th>Password</th><td><code>", "</code>")
	if len(temp) != 24 {
		t.Fatalf("temporary password not shown: %q", temp)
	}

	rec = do(t, h, request{method: http.MethodPost, path: "/login", form: url.Values{"login": {"dave@example.com"}, "password": {temp}}})
	var dave *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			dave = c
		}
	}
	if dave == nil {
		t.Fatalf("dave can't sign in: %d", rec.Code)
	}
	if rec := do(t, h, request{path: "/", cookie: dave}); rec.Header().Get("Location") != "/account/password" {
		t.Errorf("must rotate first: %s", rec.Header().Get("Location"))
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/account/password", cookie: dave})
	own := between(rec.Body.String(), "<tr><th>Password</th><td><code>", "</code>")
	if len(own) != 24 || own == temp {
		t.Fatalf("rotation didn't show a new password")
	}
	if rec := do(t, h, request{path: "/", cookie: dave}); rec.Header().Get("Location") != "/account" {
		t.Errorf("after rotating, dave still needs a nickname: %s", rec.Header().Get("Location"))
	}

	var daveID int64
	srv.Pool.QueryRow(context.Background(), "SELECT id FROM users WHERE email = 'dave@example.com'").Scan(&daveID)
	if body := do(t, h, request{path: "/admin/users", cookie: admin}).Body.String(); !strings.Contains(body, "dave@example.com") {
		t.Error("user list doesn't show dave")
	}
	do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/admin/users/%d/ban", daveID), cookie: admin, form: url.Values{"banned": {"true"}}})
	if rec := do(t, h, request{path: "/account", cookie: dave}); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("banned user still signed in: %d", rec.Code)
	}
}

func TestBoardSectionFilter(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	cookie := login(t, h)
	addPublished(t, srv, "Security story", "x", "7 days")
	ctx := context.Background()
	if _, err := srv.Pool.Exec(ctx, `INSERT INTO stories (status, title, section_id, published_at, expires_at)
		VALUES ('published', 'Business story', (SELECT id FROM sections WHERE slug = 'business'), now(), now() + interval '1 day')`); err != nil {
		t.Fatal(err)
	}

	body := do(t, h, request{path: "/?filter=1&sections=business", cookie: cookie}).Body.String()
	if !strings.Contains(body, "Business story") || strings.Contains(body, "Security story") {
		t.Error("filter not applied")
	}
	body = do(t, h, request{path: "/", cookie: cookie}).Body.String()
	if strings.Contains(body, "Security story") {
		t.Error("filter not saved as the default")
	}
	body = do(t, h, request{path: "/?filter=1", cookie: cookie}).Body.String()
	if !strings.Contains(body, "Security story") || !strings.Contains(body, "Business story") {
		t.Error("clearing the filter should show everything")
	}
}

func TestSSORoutesWithoutConfig(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	if rec := get(t, h, "/auth/sso"); rec.Code != http.StatusNotFound {
		t.Errorf("SSO start without config: %d", rec.Code)
	}
	if strings.Contains(get(t, h, "/login").Body.String(), "Sign in with SSO") {
		t.Error("SSO button shown without config")
	}
	if body := get(t, h, "/login?error=sso_domain").Body.String(); !strings.Contains(body, "Only company email addresses") {
		t.Error("known error code not shown")
	}
	if body := get(t, h, "/login?error=Call+0800+now").Body.String(); strings.Contains(body, "0800") {
		t.Error("free text from the URL must not be shown")
	}
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return ""
	}
	return s[:j]
}
