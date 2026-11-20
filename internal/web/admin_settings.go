package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/maclovin/thehackerdispatch/internal/publish"
	"github.com/maclovin/thehackerdispatch/internal/settings"
)

func (s *Server) addAdminSettingsRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return s.requireAdmin(h) }
	mux.Handle("GET /admin/settings", admin(s.handleSettings))
	mux.Handle("POST /admin/settings", admin(s.handleSettingsSave))
	mux.Handle("GET /admin/decisions", admin(s.handleDecisions))
}

type settingsPage struct {
	Ingest        settings.Ingest
	Content       settings.Content
	Auth          settings.Auth
	Domains       string
	Branding      settings.Branding
	PollIntervals []int
	TTLDays       []int
	Saved         bool
	Errors        []string
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := settingsPage{PollIntervals: settings.PollIntervals, TTLDays: settings.TTLDays, Saved: r.URL.Query().Get("saved") == "1"}
	var err error
	if page.Ingest, err = s.Settings.Ingest(ctx); err == nil {
		if page.Content, err = s.Settings.Content(ctx); err == nil {
			if page.Auth, err = s.Settings.Auth(ctx); err == nil {
				page.Branding, err = s.Settings.Branding(ctx)
			}
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	page.Domains = strings.Join(page.Auth.AllowedEmailDomains, ", ")
	s.render(w, r, http.StatusOK, "admin_settings", "Settings", page)
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	atoi := func(name string) int {
		v, _ := strconv.Atoi(r.PostFormValue(name))
		return v
	}
	ingest := settings.Ingest{PollIntervalMinutes: atoi("poll_interval_minutes")}
	content := settings.Content{TTLDays: atoi("ttl_days"), DailyCap: atoi("daily_cap"), MinScore: atoi("min_score"),
		MinGapMinutes: atoi("min_gap_minutes")}
	authSettings := settings.Auth{
		AllowedEmailDomains:  settings.ParseDomains(r.PostFormValue("allowed_domains")),
		SessionLifetimeHours: atoi("session_lifetime_hours"),
		MaxFailedLogins:      atoi("max_failed_logins"),
		LoginWaitMinutes:     atoi("login_wait_minutes"),
	}
	branding := settings.Branding{
		DisplayName:       strings.TrimSpace(r.PostFormValue("display_name")),
		PrimaryColor:      strings.ToLower(strings.TrimSpace(r.PostFormValue("primary_color"))),
		LinkColorLight:    strings.ToLower(strings.TrimSpace(r.PostFormValue("link_color_light"))),
		VisitedColorLight: strings.ToLower(strings.TrimSpace(r.PostFormValue("visited_color_light"))),
		LinkColorDark:     strings.ToLower(strings.TrimSpace(r.PostFormValue("link_color_dark"))),
		VisitedColorDark:  strings.ToLower(strings.TrimSpace(r.PostFormValue("visited_color_dark"))),
	}

	var problems []string
	if len(branding.DisplayName) > 60 {
		problems = append(problems, "The display name is limited to 60 characters.")
	}
	for _, c := range []string{branding.PrimaryColor, branding.LinkColorLight, branding.VisitedColorLight,
		branding.LinkColorDark, branding.VisitedColorDark} {
		if !settings.ValidColor(c) {
			problems = append(problems, "Colors must look like #1a2b3c (got "+strconv.Quote(c)+").")
		}
	}
	if len(problems) > 0 {
		s.render(w, r, http.StatusUnprocessableEntity, "admin_settings", "Settings", settingsPage{
			Ingest: ingest, Content: content, Auth: authSettings, Branding: branding,
			Domains: r.PostFormValue("allowed_domains"), PollIntervals: settings.PollIntervals,
			TTLDays: settings.TTLDays, Errors: problems,
		})
		return
	}

	me, _ := currentUser(r)
	ctx := r.Context()
	err := s.Settings.SetIngest(ctx, ingest, me.Actor())
	if err == nil {
		err = s.Settings.SetContent(ctx, content, me.Actor())
	}
	if err == nil {
		err = s.Settings.SetAuth(ctx, authSettings, me.Actor())
	}
	if err == nil {
		err = s.Settings.SetBranding(ctx, branding, me.Actor())
	}
	if err == nil {
		if content, err = s.Settings.Content(ctx); err == nil {
			err = publish.ApplyTTL(ctx, s.Pool, content)
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/settings?saved=1", http.StatusSeeOther)
}

type decisionRow struct {
	FetchedAt time.Time
	Source    string
	Title     string
	URL       string
	Status    string
	Decision  string
	Reason    string
	StoryID   *int64
}

type decisionsPage struct {
	Rows []decisionRow
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
		SELECT r.fetched_at, src.name, r.title, r.url, r.status, coalesce(r.decision, ''), r.reason, r.story_id
		FROM raw_items r JOIN sources src ON src.id = r.source_id
		ORDER BY r.fetched_at DESC, r.id DESC
		LIMIT 300`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[decisionRow])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin_decisions", "Decisions", decisionsPage{Rows: list})
}
