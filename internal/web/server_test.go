package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/auth"
	"github.com/maclovin/thehackerdispatch/internal/discuss"
	"github.com/maclovin/thehackerdispatch/internal/fetch"
	"github.com/maclovin/thehackerdispatch/internal/ingest"
	"github.com/maclovin/thehackerdispatch/internal/llm"
	"github.com/maclovin/thehackerdispatch/internal/notify"
	"github.com/maclovin/thehackerdispatch/internal/prompts"
	"github.com/maclovin/thehackerdispatch/internal/secretbox"
	"github.com/maclovin/thehackerdispatch/internal/settings"
	"github.com/maclovin/thehackerdispatch/internal/sources"
	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type okModel struct{}

func (okModel) Generate(context.Context, string, llm.Request) (llm.Response, error) {
	return llm.Response{Outcome: llm.OutcomeOK, JSON: []byte(`{"ok":true}`)}, nil
}

const (
	testEmail    = "alice@example.com"
	testPassword = "the-right-password"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	pool := storetest.New(t)
	st := settings.New(pool)
	if err := st.SetAuth(context.Background(), settings.Auth{AllowedEmailDomains: []string{"example.com"}}, audit.Actor{Label: "test"}); err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(pool, st, t.TempDir()+"/master-password", discard)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	storetest.MustExec(t, pool, `INSERT INTO users (auth_mode, email, nickname, password_hash, role)
		VALUES ('local', $1, 'alice', $2, 'member')`, testEmail, hash)

	base, _ := url.Parse("http://news.test")
	box, _ := secretbox.New(bytes.Repeat([]byte{1}, 32))
	srv, err := NewServer(Deps{
		Logger:   discard,
		Pool:     pool,
		Settings: st,
		Auth:     authService,
		Sources:  sources.New(pool),
		Ingester: ingest.New(pool, fetch.New(fetch.Options{UserAgent: "test", AllowPrivate: true}), discard),
		LLM: llm.NewStore(pool, box, discard, func(context.Context, string, string, string) (llm.Provider, error) {
			return okModel{}, nil
		}),
		Prompts: prompts.New(pool),
		Discuss: discuss.New(pool),
		Notify:  notify.New(pool, st, nil, base, discard),
		Box:     box,
		BaseURL: base,
		Build:   BuildInfo{Version: "v1.2.3", Commit: "abc123"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

type request struct {
	method, path string
	form         url.Values
	cookie       *http.Cookie
	header       map[string]string
}

func do(t *testing.T, h http.Handler, req request) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if req.form != nil {
		body = strings.NewReader(req.form.Encode())
	}
	method := req.method
	if method == "" {
		method = http.MethodGet
	}
	r := httptest.NewRequest(method, req.path, body)
	if req.form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if req.cookie != nil {
		r.AddCookie(req.cookie)
	}
	for k, v := range req.header {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	return do(t, h, request{path: path})
}

func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := do(t, h, request{method: http.MethodPost, path: "/login",
		form: url.Values{"login": {testEmail}, "password": {testPassword}, "next": {"/"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: status = %d, body = %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("login: no session cookie")
	return nil
}

func TestHealthz(t *testing.T) {
	srv, err := NewServer(Deps{Logger: discard, Build: BuildInfo{Version: "v1.2.3", Commit: "abc123"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, srv.HealthHandler(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["version"] != "v1.2.3" || body["commit"] != "abc123" {
		t.Errorf("body = %v", body)
	}
}

func TestReadyz(t *testing.T) {
	srv := newTestServer(t)
	if rec := get(t, srv.Handler(), "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestReadyzFailsWhileDraining(t *testing.T) {
	srv, err := NewServer(Deps{Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	srv.Drain()
	if rec := get(t, srv.HealthHandler(), "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHealthHandlerServesOnlyHealth(t *testing.T) {
	srv := newTestServer(t)
	if rec := get(t, srv.HealthHandler(), "/login"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /login on the health-only handler: status = %d, want 404", rec.Code)
	}
}

func TestBoardRequiresLogin(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	rec := get(t, h, "/")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("signed out: status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = do(t, h, request{path: "/", header: map[string]string{"HX-Request": "true"}})
	if rec.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("htmx signed out: HX-Redirect = %q", rec.Header().Get("HX-Redirect"))
	}

	cookie := login(t, h)
	if cookie.HttpOnly != true || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie flags: %+v", cookie)
	}
	rec = do(t, h, request{path: "/", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("signed in: status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<title>The Hacker Dispatch</title>", "alice", `action="/logout"`, `src="/static/htmx.min.js"`} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing %q", want)
		}
	}
	if strings.Contains(body, `href="/admin"`) {
		t.Error("member sees the admin link")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestLoginErrors(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	rec := do(t, h, request{method: http.MethodPost, path: "/login",
		form: url.Values{"login": {testEmail}, "password": {"wrong"}}})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Invalid email or password.") {
		t.Errorf("wrong password: status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "wrong") {
		t.Error("the submitted password is echoed back")
	}
}

func TestLogout(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	cookie := login(t, h)

	rec := do(t, h, request{method: http.MethodPost, path: "/logout", cookie: cookie})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: status = %d", rec.Code)
	}
	if rec := do(t, h, request{path: "/", cookie: cookie}); rec.Code != http.StatusSeeOther {
		t.Errorf("old cookie still works after logout: status = %d", rec.Code)
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	srv := newTestServer(t)
	rec := do(t, srv.Handler(), request{method: http.MethodPost, path: "/login",
		form:   url.Values{"login": {testEmail}, "password": {testPassword}},
		header: map[string]string{"Sec-Fetch-Site": "cross-site"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: status = %d, want 403", rec.Code)
	}
}

func TestSafeNext(t *testing.T) {
	tests := map[string]string{
		"":                     "/",
		"/":                    "/",
		"/s/42?x=1":            "/s/42?x=1",
		"https://evil.example": "/",
		"//evil.example":       "/",
		`/\evil.example`:       "/",
	}
	for in, want := range tests {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBrandingFromSettings(t *testing.T) {
	srv := newTestServer(t)
	storetest.MustExec(t, srv.Pool, `INSERT INTO settings (key, value) VALUES ('branding',
		'{"display_name": "MyInc News", "primary_color": "#000080", "link_color_light": "red;}"}')`)

	if body := get(t, srv.Handler(), "/login").Body.String(); !strings.Contains(body, "<title>Sign in | MyInc News</title>") {
		t.Errorf("display name not applied:\n%s", body)
	}
	css := get(t, srv.Handler(), "/theme.css").Body.String()
	if !strings.Contains(css, "--primary: #000080; --on-primary: #ffffff;") {
		t.Errorf("primary color not applied:\n%s", css)
	}
	if strings.Contains(css, "red;}") || !strings.Contains(css, "--link: #0000ee;") {
		t.Errorf("invalid link color was not replaced by the default:\n%s", css)
	}
}

func TestUserTheme(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	cookie := login(t, h)
	if body := do(t, h, request{path: "/", cookie: cookie}).Body.String(); strings.Contains(body, "data-theme") {
		t.Error("theme 'system' should not set data-theme")
	}
	if _, err := srv.Pool.Exec(context.Background(), "UPDATE users SET theme = 'dark'"); err != nil {
		t.Fatal(err)
	}
	if body := do(t, h, request{path: "/", cookie: cookie}).Body.String(); !strings.Contains(body, `data-theme="dark"`) {
		t.Error("theme 'dark' not applied")
	}
}

func TestStaticFiles(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/static/style.css", "/static/htmx.min.js", "/theme.css"} {
		if rec := get(t, srv.Handler(), path); rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d", path, rec.Code)
		}
	}
}

func TestTextColorOn(t *testing.T) {
	tests := []struct{ bg, want string }{
		{"#ff6600", "#000000"},
		{"#000080", "#ffffff"},
		{"#ffffff", "#000000"},
		{"#1a1a1a", "#ffffff"},
		{"not-a-color", "#000000"},
	}
	for _, tt := range tests {
		if got := textColorOn(tt.bg); got != tt.want {
			t.Errorf("textColorOn(%q) = %q, want %q", tt.bg, got, tt.want)
		}
	}
}
