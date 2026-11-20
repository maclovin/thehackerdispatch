package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/maclovin/thehackerdispatch/internal/ingest"
	"github.com/maclovin/thehackerdispatch/internal/jobs"
	"github.com/maclovin/thehackerdispatch/internal/sources"
)

func (s *Server) addAdminRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return s.requireAdmin(h) }
	mux.Handle("GET /admin", admin(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/sources", http.StatusSeeOther)
	}))
	mux.Handle("GET /admin/sources", admin(s.handleSources))
	mux.Handle("GET /admin/sources/new", admin(s.handleSourceNew))
	mux.Handle("POST /admin/sources", admin(s.handleSourceCreate))
	mux.Handle("POST /admin/sources/test", admin(s.handleSourceTest))
	mux.Handle("POST /admin/sources/fetch", admin(s.handleFetchNow))
	mux.Handle("GET /admin/sources/import", admin(s.handleSourceImportPage))
	mux.Handle("POST /admin/sources/import", admin(s.handleSourceImport))
	mux.Handle("GET /admin/sources/{id}", admin(s.handleSourceEdit))
	mux.Handle("POST /admin/sources/{id}", admin(s.handleSourceUpdate))
	mux.Handle("POST /admin/sources/{id}/enabled", admin(s.handleSourceEnabled))
	mux.Handle("POST /admin/sources/{id}/delete", admin(s.handleSourceDelete))
}

type sourcesPage struct {
	Sources []sources.Source
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	list, err := s.Sources.List(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin_sources", "Sources", sourcesPage{Sources: list})
}

type sourceForm struct {
	ID       int64
	Input    sources.Input
	Source   *sources.Source
	Sections []sources.Section
	Errors   []string
}

func (s *Server) renderSourceForm(w http.ResponseWriter, r *http.Request, status int, form sourceForm) {
	sections, err := s.Sources.Sections(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	form.Sections = sections
	title := "Add source"
	if form.ID != 0 {
		title = "Edit source"
	}
	s.render(w, r, status, "admin_source_form", title, form)
}

func (s *Server) handleSourceNew(w http.ResponseWriter, r *http.Request) {
	s.renderSourceForm(w, r, http.StatusOK, sourceForm{Input: sources.Input{Enabled: true}})
}

func (s *Server) handleSourceCreate(w http.ResponseWriter, r *http.Request) {
	in := sourceInput(r)
	user, _ := currentUser(r)
	_, err := s.Sources.Create(r.Context(), in, user.Actor())
	if err != nil {
		s.sourceFormError(w, r, sourceForm{Input: in}, err)
		return
	}
	if err := jobs.RunSoon(r.Context(), s.Pool, ingest.JobName); err != nil {
		s.Logger.Warn("schedule ingestion", "err", err)
	}
	http.Redirect(w, r, "/admin/sources", http.StatusSeeOther)
}

func (s *Server) handleFetchNow(w http.ResponseWriter, r *http.Request) {
	if err := jobs.RunSoon(r.Context(), s.Pool, ingest.JobName); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/sources", http.StatusSeeOther)
}

func (s *Server) handleSourceEdit(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sourceFromPath(w, r)
	if !ok {
		return
	}
	in := sources.Input{Name: src.Name, URL: src.URL, SectionID: src.SectionID, Enabled: src.Enabled, UserAgent: src.UserAgent}
	s.renderSourceForm(w, r, http.StatusOK, sourceForm{ID: src.ID, Input: in, Source: &src})
}

func (s *Server) handleSourceUpdate(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sourceFromPath(w, r)
	if !ok {
		return
	}
	in := sourceInput(r)
	user, _ := currentUser(r)
	if err := s.Sources.Update(r.Context(), src.ID, in, user.Actor()); err != nil {
		s.sourceFormError(w, r, sourceForm{ID: src.ID, Input: in, Source: &src}, err)
		return
	}
	http.Redirect(w, r, "/admin/sources", http.StatusSeeOther)
}

func (s *Server) handleSourceEnabled(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sourceFromPath(w, r)
	if !ok {
		return
	}
	user, _ := currentUser(r)
	if err := s.Sources.SetEnabled(r.Context(), src.ID, r.PostFormValue("enabled") == "true", user.Actor()); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/sources", http.StatusSeeOther)
}

func (s *Server) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sourceFromPath(w, r)
	if !ok {
		return
	}
	user, _ := currentUser(r)
	if err := s.Sources.Delete(r.Context(), src.ID, user.Actor()); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/admin/sources")
}

const maxOPMLBytes = 2 << 20

type importPage struct {
	Sections  []sources.Section
	SectionID int64
	Enabled   bool
	Error     string
	Result    *sources.ImportResult
}

func (s *Server) renderImport(w http.ResponseWriter, r *http.Request, status int, page importPage) {
	var err error
	if page.Sections, err = s.Sources.Sections(r.Context()); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "admin_source_import", "Import sources", page)
}

func (s *Server) handleSourceImportPage(w http.ResponseWriter, r *http.Request) {
	s.renderImport(w, r, http.StatusOK, importPage{Enabled: true})
}

func (s *Server) handleSourceImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOPMLBytes)
	page := importPage{Enabled: r.PostFormValue("enabled") == "on"}
	page.SectionID, _ = strconv.ParseInt(r.PostFormValue("section_id"), 10, 64)
	file, _, err := r.FormFile("opml")
	if err != nil {
		page.Error = "Pick an OPML file, up to 2 MB."
		s.renderImport(w, r, http.StatusUnprocessableEntity, page)
		return
	}
	defer file.Close()
	feeds, err := sources.ParseOPML(file)
	if err != nil {
		page.Error = err.Error()
		s.renderImport(w, r, http.StatusUnprocessableEntity, page)
		return
	}
	user, _ := currentUser(r)
	result, err := s.Sources.Import(r.Context(), feeds, page.SectionID, page.Enabled, user.Actor())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(result.Added) > 0 && page.Enabled {
		if err := jobs.RunSoon(r.Context(), s.Pool, ingest.JobName); err != nil {
			s.Logger.Warn("schedule ingestion", "err", err)
		}
	}
	page.Result = &result
	s.renderImport(w, r, http.StatusOK, page)
}

type sourcePreview struct {
	Count int
	Items []ingest.Item
	Error string
}

func (s *Server) handleSourceTest(w http.ResponseWriter, r *http.Request) {
	items, err := s.Ingester.Preview(r.Context(), strings.TrimSpace(r.PostFormValue("url")),
		strings.TrimSpace(r.PostFormValue("user_agent")))
	preview := sourcePreview{Count: len(items), Items: items}
	if err != nil {
		preview.Error = err.Error()
	}
	if len(preview.Items) > 10 {
		preview.Items = preview.Items[:10]
	}
	s.renderFragment(w, http.StatusOK, "source_preview", preview)
}

func sourceInput(r *http.Request) sources.Input {
	sectionID, _ := strconv.ParseInt(r.PostFormValue("section_id"), 10, 64)
	return sources.Input{
		Name:      r.PostFormValue("name"),
		URL:       r.PostFormValue("url"),
		SectionID: sectionID,
		Enabled:   r.PostFormValue("enabled") == "on",
		UserAgent: r.PostFormValue("user_agent"),
	}
}

func (s *Server) sourceFormError(w http.ResponseWriter, r *http.Request, form sourceForm, err error) {
	var invalid interface{ Unwrap() []error }
	switch {
	case errors.Is(err, sources.ErrDuplicateURL), errors.Is(err, sources.ErrUnknownSection):
		form.Errors = []string{err.Error()}
	case errors.As(err, &invalid):
		for _, e := range invalid.Unwrap() {
			form.Errors = append(form.Errors, e.Error())
		}
	default:
		s.serverError(w, r, err)
		return
	}
	s.renderSourceForm(w, r, http.StatusUnprocessableEntity, form)
}

func (s *Server) sourceFromPath(w http.ResponseWriter, r *http.Request) (sources.Source, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Source not found.")
		return sources.Source{}, false
	}
	src, err := s.Sources.Get(r.Context(), id)
	if errors.Is(err, sources.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Source not found.")
		return sources.Source{}, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return sources.Source{}, false
	}
	return src, true
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.Logger.Error("request failed", "path", r.URL.Path, "err", err)
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong.")
}

func redirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
