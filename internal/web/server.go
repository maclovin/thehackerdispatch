package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/auth"
	"github.com/maclovin/thehackerdispatch/internal/discuss"
	"github.com/maclovin/thehackerdispatch/internal/ingest"
	"github.com/maclovin/thehackerdispatch/internal/llm"
	"github.com/maclovin/thehackerdispatch/internal/notify"
	"github.com/maclovin/thehackerdispatch/internal/prompts"
	"github.com/maclovin/thehackerdispatch/internal/secretbox"
	"github.com/maclovin/thehackerdispatch/internal/settings"
	"github.com/maclovin/thehackerdispatch/internal/sources"
)

//go:embed templates static
var assets embed.FS

type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type Deps struct {
	Logger   *slog.Logger
	Pool     *pgxpool.Pool
	Settings *settings.Store
	Auth     *auth.Service
	Sources  *sources.Store
	Ingester *ingest.Ingester
	LLM      *llm.Store
	Prompts  *prompts.Store
	Discuss  *discuss.Store
	Notify   *notify.Notifier
	SSO      *auth.SSO
	Box      *secretbox.Box
	BaseURL  *url.URL
	Build    BuildInfo
}

type Server struct {
	Deps
	pages *renderer
}

func NewServer(deps Deps) (*Server, error) {
	pages, err := newRenderer()
	if err != nil {
		return nil, err
	}
	return &Server{Deps: deps, pages: pages}, nil
}

func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	s.addHealthRoutes(mux)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /theme.css", s.handleThemeCSS)

	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.Handle("POST /logout", s.requireUser(http.HandlerFunc(s.handleLogout)))
	mux.HandleFunc("GET /auth/sso", s.handleSSOStart)
	mux.HandleFunc("GET /auth/callback", s.handleSSOCallback)

	mux.Handle("GET /{$}", s.requireUser(http.HandlerFunc(s.handleBoard)))
	mux.Handle("GET /s/{id}", s.requireUser(http.HandlerFunc(s.handleStory)))
	s.addCommentRoutes(mux)
	s.addAccountRoutes(mux)
	s.addAdminRoutes(mux)
	s.addAdminLLMRoutes(mux)
	s.addAdminUserRoutes(mux)
	s.addAdminStoryRoutes(mux)
	s.addAdminSettingsRoutes(mux)
	s.addAdminSectionRoutes(mux)
	s.addAdminChannelRoutes(mux)

	// CSRF protection from the browser's Sec-Fetch-Site and Origin headers; no tokens needed.
	cop := http.NewCrossOriginProtection()
	if s.BaseURL != nil {
		if err := cop.AddTrustedOrigin(s.BaseURL.Scheme + "://" + s.BaseURL.Host); err != nil {
			s.Logger.Warn("trust BASE_URL origin", "err", err)
		}
	}
	return s.middleware(cop.Handler(mux))
}

func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	s.addHealthRoutes(mux)
	return s.middleware(mux)
}

func (s *Server) addHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Build.Version, "commit": s.Build.Commit})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Pool.Ping(ctx); err != nil {
		s.Logger.Warn("readiness check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleThemeCSS(w http.ResponseWriter, r *http.Request) {
	b, err := s.Settings.Branding(r.Context())
	if err != nil {
		s.Logger.Error("load branding", "err", err)
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, `:root { --primary: %s; --on-primary: %s; --link: %s; --visited: %s; }
@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { --link: %s; --visited: %s; } }
:root[data-theme="dark"] { --link: %s; --visited: %s; }
`, b.PrimaryColor, textColorOn(b.PrimaryColor), b.LinkColorLight, b.VisitedColorLight,
		b.LinkColorDark, b.VisitedColorDark, b.LinkColorDark, b.VisitedColorDark)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
