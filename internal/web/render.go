package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/maclovin/thehackerdispatch/internal/auth"
	"github.com/maclovin/thehackerdispatch/internal/settings"
)

type renderer struct {
	pages     map[string]*template.Template
	fragments map[string]*template.Template
}

var funcs = template.FuncMap{
	"ago":       ago,
	"since":     func(t time.Time) string { return ago(&t) },
	"expiresIn": expiresIn,
	"add":       func(a, b int) int { return a + b },
	"div":       func(a, b int) int { return a / b },
	"deref":     func(p *int64) int64 { return *p },
}

func newRenderer() (*renderer, error) {
	r := &renderer{pages: map[string]*template.Template{}, fragments: map[string]*template.Template{}}
	sets := []struct {
		glob   string
		shared []string
		into   map[string]*template.Template
	}{
		{"templates/pages/*.html", []string{"templates/layout.html", "templates/partials/*.html"}, r.pages},
		{"templates/fragments/*.html", []string{"templates/partials/*.html"}, r.fragments},
	}
	for _, set := range sets {
		files, err := fs.Glob(assets, set.glob)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			t, err := template.New("").Funcs(funcs).ParseFS(assets, append(set.shared, f)...)
			if err != nil {
				return nil, fmt.Errorf("parse template %s: %w", f, err)
			}
			set.into[strings.TrimSuffix(path.Base(f), ".html")] = t
		}
	}
	return r, nil
}

type pageData struct {
	Title   string
	Brand   settings.Branding
	User    *auth.User
	Theme   string
	TTLDays int
	Content any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, title string, content any) {
	t, ok := s.pages.pages[page]
	if !ok {
		s.Logger.Error("unknown page template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	brand, err := s.Settings.Branding(r.Context())
	if err != nil {
		s.Logger.Error("load branding", "err", err)
	}
	contentSettings, err := s.Settings.Content(r.Context())
	if err != nil {
		s.Logger.Error("load content settings", "err", err)
	}
	data := pageData{Title: title, Brand: brand, TTLDays: contentSettings.TTLDays, Content: content}
	if u, ok := currentUser(r); ok {
		data.User = &u
		if u.Theme != "system" {
			data.Theme = u.Theme
		}
	}
	s.execute(w, status, t, "layout", data)
}

func (s *Server) renderFragment(w http.ResponseWriter, status int, name string, data any) {
	t, ok := s.pages.fragments[name]
	if !ok {
		s.Logger.Error("unknown fragment template", "fragment", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.execute(w, status, t, "fragment", data)
}

func (s *Server) execute(w http.ResponseWriter, status int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.Logger.Error("render template", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, r, status, "error", http.StatusText(status), message)
}

func ago(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	unit := func(n int, name string) string {
		if n == 1 {
			return "1 " + name + " ago"
		}
		return strconv.Itoa(n) + " " + name + "s ago"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return unit(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return unit(int(d/time.Hour), "hour")
	default:
		return unit(int(d/(24*time.Hour)), "day")
	}
}

func textColorOn(bg string) string {
	if !settings.ValidColor(bg) {
		return "#000000"
	}
	channel := func(hex string) float64 {
		v, _ := strconv.ParseUint(hex, 16, 8)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	l := 0.2126*channel(bg[1:3]) + 0.7152*channel(bg[3:5]) + 0.0722*channel(bg[5:7])
	if l > 0.179 {
		return "#000000"
	}
	return "#ffffff"
}
