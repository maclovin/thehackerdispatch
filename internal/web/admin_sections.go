package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/maclovin/thehackerdispatch/internal/curate"
	"github.com/maclovin/thehackerdispatch/internal/notify"
	"github.com/maclovin/thehackerdispatch/internal/prompts"
	"github.com/maclovin/thehackerdispatch/internal/publish"
	"github.com/maclovin/thehackerdispatch/internal/sources"
)

func (s *Server) addAdminSectionRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return s.requireAdmin(h) }
	mux.Handle("GET /admin/sections", admin(s.handleSections))
	mux.Handle("POST /admin/sections", admin(s.handleSectionCreate))
	mux.Handle("GET /admin/sections/{id}", admin(s.handleSection))
	mux.Handle("POST /admin/sections/{id}", admin(s.handleSectionRename))
	mux.Handle("POST /admin/sections/{id}/move", admin(s.handleSectionMove))
	mux.Handle("POST /admin/sections/{id}/delete", admin(s.handleSectionDelete))
	mux.Handle("POST /admin/sections/{id}/prompt", admin(s.handleSectionPromptSave))
	mux.Handle("GET /admin/prompt", admin(s.handleGlobalPrompt))
	mux.Handle("POST /admin/prompt", admin(s.handleGlobalPromptSave))
	mux.Handle("POST /admin/prompts/{id}/restore", admin(s.handlePromptRestore))
}

type promptEditor struct {
	Action  string
	Unsaved string
	Body    string
	Current prompts.Version
	History []prompts.Version
	Saved   bool
	Errors  []string
}

func (s *Server) loadEditor(r *http.Request, scope int64, action, unsaved string, draft *string) (promptEditor, error) {
	ed := promptEditor{Action: action, Unsaved: unsaved, Saved: r.URL.Query().Get("saved") == "1"}
	var err error
	if ed.Current, err = s.Prompts.Current(r.Context(), scope); err != nil {
		return ed, err
	}
	if ed.History, err = s.Prompts.History(r.Context(), scope); err != nil {
		return ed, err
	}
	ed.Body = ed.Current.Body
	if draft != nil {
		ed.Body = *draft
	}
	return ed, nil
}

func (s *Server) savePrompt(w http.ResponseWriter, r *http.Request, scope int64, back string,
	again func(draft string, problem error)) {
	user, _ := currentUser(r)
	body := r.PostFormValue("body")
	_, err := s.Prompts.Save(r.Context(), scope, body, user.Actor())
	switch {
	case errors.Is(err, prompts.ErrEmptyGlobal), errors.Is(err, prompts.ErrTooLong), errors.Is(err, prompts.ErrConflict):
		again(body, err)
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, back+"?saved=1", http.StatusSeeOther)
	}
}

type promptPage struct {
	CurationContract string
	WritingContract  string
	Editor           promptEditor
}

func (s *Server) handleGlobalPrompt(w http.ResponseWriter, r *http.Request) {
	s.renderGlobalPrompt(w, r, http.StatusOK, nil, nil)
}

func (s *Server) renderGlobalPrompt(w http.ResponseWriter, r *http.Request, status int, draft *string, problem error) {
	ed, err := s.loadEditor(r, prompts.Global, "/admin/prompt", "built-in default, not saved yet", draft)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if problem != nil {
		ed.Errors, ed.Saved = []string{problem.Error()}, false
	}
	s.render(w, r, status, "admin_prompt", "Prompt", promptPage{
		CurationContract: curate.Contract,
		WritingContract:  publish.Contract,
		Editor:           ed,
	})
}

func (s *Server) handleGlobalPromptSave(w http.ResponseWriter, r *http.Request) {
	s.savePrompt(w, r, prompts.Global, "/admin/prompt", func(draft string, problem error) {
		s.renderGlobalPrompt(w, r, http.StatusUnprocessableEntity, &draft, problem)
	})
}

func (s *Server) handlePromptRestore(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	user, _ := currentUser(r)
	scope, err := s.Prompts.Restore(r.Context(), id, user.Actor())
	switch {
	case errors.Is(err, prompts.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Prompt version not found.")
	case err != nil:
		s.serverError(w, r, err)
	case scope == prompts.Global:
		http.Redirect(w, r, "/admin/prompt?saved=1", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/admin/sections/"+strconv.FormatInt(scope, 10)+"?saved=1", http.StatusSeeOther)
	}
}

type sectionsPage struct {
	Sections []sources.SectionSummary
	Input    sources.SectionInput
	Errors   []string
}

func (s *Server) handleSections(w http.ResponseWriter, r *http.Request) {
	s.renderSections(w, r, http.StatusOK, sectionsPage{})
}

func (s *Server) renderSections(w http.ResponseWriter, r *http.Request, status int, page sectionsPage) {
	var err error
	if page.Sections, err = s.Sources.SectionSummaries(r.Context()); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "admin_sections", "Sections", page)
}

func (s *Server) handleSectionCreate(w http.ResponseWriter, r *http.Request) {
	in := sources.SectionInput{Slug: r.PostFormValue("slug"), Name: r.PostFormValue("name")}
	user, _ := currentUser(r)
	id, err := s.Sources.CreateSection(r.Context(), in, user.Actor())
	if problems := sectionProblems(err); problems != nil {
		s.renderSections(w, r, http.StatusUnprocessableEntity, sectionsPage{Input: in, Errors: problems})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/sections/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleSectionMove(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	delta := 1
	if r.PostFormValue("dir") == "up" {
		delta = -1
	}
	err := s.Sources.MoveSection(r.Context(), id, delta)
	if err != nil && !errors.Is(err, sources.ErrSectionNotFound) {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/sections", http.StatusSeeOther)
}

type sectionPage struct {
	Section  sources.SectionSummary
	Name     string
	Channels []notify.Channel
	Editor   promptEditor
	Errors   []string
}

func (s *Server) handleSection(w http.ResponseWriter, r *http.Request) {
	s.renderSection(w, r, http.StatusOK, nil, nil, nil)
}

func (s *Server) renderSection(w http.ResponseWriter, r *http.Request, status int, name, draft *string, problems []string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	all, err := s.Sources.SectionSummaries(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	page := sectionPage{Errors: problems}
	found := false
	for _, sec := range all {
		if sec.ID == id {
			page.Section, found = sec, true
		}
	}
	if !found {
		s.renderError(w, r, http.StatusNotFound, "Section not found.")
		return
	}
	page.Name = page.Section.Name
	if name != nil {
		page.Name = *name
	}
	if page.Channels, err = s.Notify.Channels(r.Context()); err != nil {
		s.serverError(w, r, err)
		return
	}
	page.Editor, err = s.loadEditor(r, id, "/admin/sections/"+strconv.FormatInt(id, 10)+"/prompt",
		"no prompt yet: this section uses only the global prompt", draft)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if problems != nil {
		page.Editor.Saved = false
	}
	s.render(w, r, status, "admin_section", page.Section.Name, page)
}

func (s *Server) handleSectionRename(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	name := r.PostFormValue("name")
	user, _ := currentUser(r)
	err := s.Sources.RenameSection(r.Context(), id, name, user.Actor())
	if problems := sectionProblems(err); problems != nil {
		s.renderSection(w, r, http.StatusUnprocessableEntity, &name, nil, problems)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/sections", http.StatusSeeOther)
}

func (s *Server) handleSectionPromptSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := s.Sources.Section(r.Context(), id); err != nil {
		s.renderError(w, r, http.StatusNotFound, "Section not found.")
		return
	}
	s.savePrompt(w, r, id, "/admin/sections/"+strconv.FormatInt(id, 10), func(draft string, problem error) {
		s.renderSection(w, r, http.StatusUnprocessableEntity, nil, &draft, []string{problem.Error()})
	})
}

func (s *Server) handleSectionDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	user, _ := currentUser(r)
	err := s.Sources.DeleteSection(r.Context(), id, user.Actor())
	switch {
	case errors.Is(err, sources.ErrSectionNotFound):
		s.renderError(w, r, http.StatusNotFound, "Section not found.")
	case errors.Is(err, sources.ErrSectionInUse), errors.Is(err, sources.ErrLastSection):
		s.renderError(w, r, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, r, err)
	default:
		redirect(w, r, "/admin/sections")
	}
}

func sectionProblems(err error) []string {
	var invalid interface{ Unwrap() []error }
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sources.ErrDuplicateSection):
		return []string{err.Error()}
	case errors.As(err, &invalid):
		var problems []string
		for _, e := range invalid.Unwrap() {
			problems = append(problems, e.Error())
		}
		return problems
	}
	return nil
}
