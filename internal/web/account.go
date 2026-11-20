package web

import (
	"errors"
	"net/http"

	"github.com/maclovin/thehackerdispatch/internal/auth"
)

func (s *Server) addAccountRoutes(mux *http.ServeMux) {
	user := func(h http.HandlerFunc) http.Handler { return s.requireUser(h) }
	mux.Handle("GET /account", user(s.handleAccount))
	mux.Handle("POST /account", user(s.handleAccountSave))
	mux.Handle("GET /account/password", user(s.handlePasswordPage))
	mux.Handle("POST /account/password", user(s.handlePasswordRotate))
	mux.Handle("POST /account/sessions/{id}/end", user(s.handleEndSession))
	mux.Handle("POST /account/sessions/end-others", user(s.handleEndOtherSessions))
}

type accountPage struct {
	Me       auth.User
	Setup    bool
	Sessions []auth.SessionInfo
	Errors   []string
	Saved    bool
	Nickname string
	Themes   []string
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, http.StatusOK, nil, "")
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, status int, problems []string, nickname string) {
	me, _ := currentUser(r)
	sessions, err := s.Auth.Sessions(r.Context(), me.ID, sessionToken(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if nickname == "" {
		nickname = me.Nickname
	}
	s.render(w, r, status, "account", "Your account", accountPage{
		Me:       me,
		Setup:    me.Nickname == "",
		Sessions: sessions,
		Errors:   problems,
		Saved:    r.URL.Query().Get("saved") == "1",
		Nickname: nickname,
		Themes:   []string{"system", "light", "dark"},
	})
}

func (s *Server) handleAccountSave(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	ctx := r.Context()
	nickname := r.PostFormValue("nickname")
	if nickname != me.Nickname {
		if err := s.Auth.SetNickname(ctx, me, nickname); err != nil {
			if errors.Is(err, auth.ErrInvalidNickname) || errors.Is(err, auth.ErrNicknameTaken) {
				s.renderAccount(w, r, http.StatusUnprocessableEntity, []string{err.Error()}, nickname)
				return
			}
			s.serverError(w, r, err)
			return
		}
	}
	if theme := r.PostFormValue("theme"); theme != "" {
		if err := s.Auth.SetTheme(ctx, me, theme); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if me.Nickname == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/account?saved=1", http.StatusSeeOther)
}

type passwordPage struct {
	Me    auth.User
	Error string
}

func (s *Server) handlePasswordPage(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	if me.AuthMode != "local" {
		s.renderError(w, r, http.StatusNotFound, "Your account signs in with SSO; its password is managed there.")
		return
	}
	s.render(w, r, http.StatusOK, "account_password", "Password", passwordPage{Me: me})
}

type shownPassword struct {
	Heading  string
	Message  string
	Login    string
	Password string
	Continue string
}

func (s *Server) handlePasswordRotate(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	password, err := s.Auth.RotatePassword(r.Context(), me, r.PostFormValue("current"), sessionToken(r))
	switch {
	case errors.Is(err, auth.ErrWrongPassword):
		s.render(w, r, http.StatusUnprocessableEntity, "account_password", "Password", passwordPage{Me: me, Error: err.Error()})
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	login := me.Email
	if me.IsMaster {
		login = auth.MasterNickname
	}
	s.renderPassword(w, r, shownPassword{
		Heading:  "Your new password",
		Message:  "Save it in your password manager now: it won't be shown again. Your other sessions were signed out.",
		Login:    login,
		Password: password,
		Continue: "/",
	})
}

func (s *Server) renderPassword(w http.ResponseWriter, r *http.Request, p shownPassword) {
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, http.StatusOK, "password_shown", p.Heading, p)
}

func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	if err := s.Auth.EndSession(r.Context(), me, r.PathValue("id")); err != nil && !errors.Is(err, auth.ErrNoSession) {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, safeNext(r.PostFormValue("back")))
}

func (s *Server) handleEndOtherSessions(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	if err := s.Auth.EndOtherSessions(r.Context(), me, sessionToken(r)); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}
