package web

import (
	"bytes"
	"errors"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"

	"github.com/maclovin/thehackerdispatch/internal/sources"
)

const boardLimit = 100

type boardStory struct {
	ID          int64
	Title       string
	Section     string
	URL         string
	PublishedAt time.Time
	ExpiresAt   *time.Time
	Critical    bool
	Sources     int
	Comments    int
}

func (s boardStory) Domain() string { return domainOf(s.URL) }

type sectionFilter struct {
	sources.Section
	Checked bool
}

type boardPage struct {
	Stories  []boardStory
	Sections []sectionFilter
	Filtered bool
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me, _ := currentUser(r)
	all, err := s.Sources.Sections(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	selected := me.PreferredSections
	if r.URL.Query().Get("filter") == "1" {
		selected = nil
		for _, slug := range r.URL.Query()["sections"] {
			if slices.ContainsFunc(all, func(sec sources.Section) bool { return sec.Slug == slug }) {
				selected = append(selected, slug)
			}
		}
		if len(selected) == len(all) {
			selected = nil
		}
		if err := s.Auth.SetPreferredSections(ctx, me, selected); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if selected == nil {
		selected = []string{}
	}

	rows, err := s.Pool.Query(ctx, `
		SELECT s.id, s.title, sec.name, coalesce(l.url, ''), s.published_at, s.expires_at, s.priority = 'critical',
		       (SELECT count(*) FROM story_links WHERE story_id = s.id), s.comment_count
		FROM stories s
		JOIN sections sec ON sec.id = s.section_id
		LEFT JOIN story_links l ON l.story_id = s.id AND l.is_primary
		WHERE s.status = 'published' AND (s.expires_at IS NULL OR s.expires_at > now())
		  AND (cardinality($2::text[]) = 0 OR sec.slug = ANY($2))
		ORDER BY s.published_at DESC, s.id DESC
		LIMIT $1`, boardLimit, selected)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	stories, err := pgx.CollectRows(rows, pgx.RowToStructByPos[boardStory])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	page := boardPage{Stories: stories, Filtered: len(selected) > 0}
	for _, sec := range all {
		page.Sections = append(page.Sections, sectionFilter{Section: sec, Checked: len(selected) == 0 || slices.Contains(selected, sec.Slug)})
	}
	s.render(w, r, http.StatusOK, "board", "", page)
}

type storyLink struct {
	URL    string
	Title  string
	Source string
}

type storyUpdate struct {
	Note      string
	URL       string
	CreatedAt time.Time
}

type storyForm struct {
	Error    string
	Draft    string
	ParentID int64
}

type storyPage struct {
	boardStory
	Summary      template.HTML
	WhyItMatters string
	TLDR         string
	Model        string
	Links        []storyLink
	Updates      []storyUpdate
	Thread       []commentNode
	Form         storyForm
}

func (s *Server) handleStory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "This story doesn't exist, or it has expired.")
		return
	}
	s.renderStory(w, r, id, http.StatusOK, storyForm{})
}

func (s *Server) renderStory(w http.ResponseWriter, r *http.Request, id int64, status int, form storyForm) {
	ctx := r.Context()
	me, _ := currentUser(r)
	var (
		p       storyPage
		summary string
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT s.id, s.title, sec.name, coalesce(l.url, ''), s.published_at, s.expires_at, s.priority = 'critical',
		       s.comment_count, s.summary_md, s.why_it_matters, s.tldr, s.model
		FROM stories s
		JOIN sections sec ON sec.id = s.section_id
		LEFT JOIN story_links l ON l.story_id = s.id AND l.is_primary
		WHERE s.id = $1 AND s.status = 'published' AND (s.expires_at IS NULL OR s.expires_at > now())`, id).
		Scan(&p.ID, &p.Title, &p.Section, &p.URL, &p.PublishedAt, &p.ExpiresAt, &p.Critical,
			&p.Comments, &summary, &p.WhyItMatters, &p.TLDR, &p.Model)
	if errors.Is(err, pgx.ErrNoRows) {
		s.renderError(w, r, http.StatusNotFound, "This story doesn't exist, or it has expired.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Summary = renderMarkdown(summary)
	p.Form = form

	rows, err := s.Pool.Query(ctx, `
		SELECT l.url, l.title, coalesce(src.name, '')
		FROM story_links l LEFT JOIN sources src ON src.id = l.source_id
		WHERE l.story_id = $1 ORDER BY l.is_primary DESC, l.added_at`, id)
	if err == nil {
		p.Links, err = pgx.CollectRows(rows, pgx.RowToStructByPos[storyLink])
	}
	if err == nil {
		p.Sources = len(p.Links)
		rows, err = s.Pool.Query(ctx, "SELECT note, url, created_at FROM story_updates WHERE story_id = $1 ORDER BY created_at", id)
	}
	if err == nil {
		p.Updates, err = pgx.CollectRows(rows, pgx.RowToStructByPos[storyUpdate])
	}
	if err == nil {
		thread, terr := s.Discuss.Thread(ctx, id, me.ID)
		p.Thread, err = buildNodes(id, thread, me), terr
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "story", p.Title, p)
}

var sanitizer = bluemonday.UGCPolicy()

func renderMarkdown(md string) template.HTML {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(md), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(md))
	}
	return template.HTML(sanitizer.SanitizeBytes(buf.Bytes()))
}

func domainOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

func expiresIn(t *time.Time) string {
	if t == nil {
		return "never expires"
	}
	d := time.Until(*t)
	switch {
	case d <= time.Hour:
		return "expires within the hour"
	case d < 48*time.Hour:
		return "expires in " + strconv.Itoa(int(math.Round(d.Hours()))) + " hours"
	default:
		return "expires in " + strconv.Itoa(int(math.Round(d.Hours()/24))) + " days"
	}
}
