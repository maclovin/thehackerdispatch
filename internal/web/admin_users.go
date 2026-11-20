package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/maclovin/thehackerdispatch/internal/auth"
)

func (s *Server) addAdminUserRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return s.requireAdmin(h) }
	mux.Handle("GET /admin/users", admin(s.handleUsers))
	mux.Handle("POST /admin/users", admin(s.handleUserCreate))
	mux.Handle("GET /admin/users/{id}", admin(s.handleUser))
	mux.Handle("POST /admin/users/{id}/reset", admin(s.handleUserReset))
	mux.Handle("POST /admin/users/{id}/mode", admin(s.handleUserMode))
	mux.Handle("POST /admin/users/{id}/role", admin(s.handleUserRole))
	mux.Handle("POST /admin/users/{id}/ban", admin(s.handleUserBan))
	mux.Handle("POST /admin/users/{id}/delete", admin(s.handleUserDelete))
	mux.Handle("POST /admin/sessions/end-all", admin(s.handleEndAllSessions))
}

type usersPage struct {
	Accounts []auth.Account
	Errors   []string
	Form     auth.NewAccount
	SSO      bool
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	s.renderUsers(w, r, http.StatusOK, nil, auth.NewAccount{AuthMode: "local", Role: "member"})
}

func (s *Server) renderUsers(w http.ResponseWriter, r *http.Request, status int, problems []string, form auth.NewAccount) {
	accounts, err := s.Auth.Accounts(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "admin_users", "Users", usersPage{Accounts: accounts, Errors: problems, Form: form, SSO: s.SSO != nil})
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	form := auth.NewAccount{
		Email:    r.PostFormValue("email"),
		RealName: r.PostFormValue("real_name"),
		Role:     r.PostFormValue("role"),
		AuthMode: r.PostFormValue("auth_mode"),
	}
	id, password, err := s.Auth.CreateAccount(r.Context(), me, form)
	if err != nil {
		if msg, known := userError(err); known {
			s.renderUsers(w, r, http.StatusUnprocessableEntity, []string{msg}, form)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if password == "" {
		http.Redirect(w, r, fmt.Sprintf("/admin/users/%d", id), http.StatusSeeOther)
		return
	}
	s.renderPassword(w, r, shownPassword{
		Heading: "Account created",
		Message: "Send these credentials to the person through a private channel. The password works once, " +
			"for 72 hours: at the first sign-in they get a new password that only they will see.",
		Login:    form.Email,
		Password: password,
		Continue: fmt.Sprintf("/admin/users/%d", id),
	})
}

type userPage struct {
	Account  auth.Account
	Sessions []auth.SessionInfo
	Error    string
	SSO      bool
	Self     bool
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	s.renderUser(w, r, http.StatusOK, "")
}

func (s *Server) renderUser(w http.ResponseWriter, r *http.Request, status int, problem string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	account, err := s.Auth.Account(r.Context(), id)
	if errors.Is(err, auth.ErrUserNotFound) {
		s.renderError(w, r, http.StatusNotFound, "User not found.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sessions, err := s.Auth.Sessions(r.Context(), id, sessionToken(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	me, _ := currentUser(r)
	s.render(w, r, status, "admin_user", "User", userPage{
		Account: account, Sessions: sessions, Error: problem, SSO: s.SSO != nil, Self: me.ID == account.ID,
	})
}

func (s *Server) userAction(w http.ResponseWriter, r *http.Request, action func(me auth.User, id int64) error) {
	me, _ := currentUser(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := action(me, id); err != nil {
		if msg, known := userError(err); known {
			s.renderUser(w, r, http.StatusUnprocessableEntity, msg)
			return
		}
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/admin/users/%d", id))
}

func (s *Server) handleUserRole(w http.ResponseWriter, r *http.Request) {
	s.userAction(w, r, func(me auth.User, id int64) error {
		return s.Auth.SetRole(r.Context(), me, id, r.PostFormValue("role"))
	})
}

func (s *Server) handleUserBan(w http.ResponseWriter, r *http.Request) {
	s.userAction(w, r, func(me auth.User, id int64) error {
		return s.Auth.SetBanned(r.Context(), me, id, r.PostFormValue("banned") == "true")
	})
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Auth.DeleteUser(r.Context(), me, id); err != nil {
		if msg, known := userError(err); known {
			s.renderUser(w, r, http.StatusUnprocessableEntity, msg)
			return
		}
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/admin/users")
}

func (s *Server) handleUserReset(w http.ResponseWriter, r *http.Request) {
	s.passwordAction(w, r, "Password reset", func(me auth.User, id int64) (string, error) {
		return s.Auth.ResetPassword(r.Context(), me, id)
	})
}

func (s *Server) handleUserMode(w http.ResponseWriter, r *http.Request) {
	s.passwordAction(w, r, "Switched to password sign-in", func(me auth.User, id int64) (string, error) {
		return s.Auth.SetAuthMode(r.Context(), me, id, r.PostFormValue("mode"))
	})
}

func (s *Server) passwordAction(w http.ResponseWriter, r *http.Request, heading string, action func(me auth.User, id int64) (string, error)) {
	me, _ := currentUser(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	password, err := action(me, id)
	if err != nil {
		if msg, known := userError(err); known {
			s.renderUser(w, r, http.StatusUnprocessableEntity, msg)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if password == "" {
		http.Redirect(w, r, fmt.Sprintf("/admin/users/%d", id), http.StatusSeeOther)
		return
	}
	account, err := s.Auth.Account(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	login := account.Email
	if account.IsMaster {
		login = auth.MasterNickname
	}
	s.renderPassword(w, r, shownPassword{
		Heading: heading,
		Message: "Send it to the person through a private channel. It works once, for 72 hours; " +
			"their sessions were signed out.",
		Login:    login,
		Password: password,
		Continue: fmt.Sprintf("/admin/users/%d", id),
	})
}

func (s *Server) handleEndAllSessions(w http.ResponseWriter, r *http.Request) {
	me, _ := currentUser(r)
	if err := s.Auth.EndAllSessions(r.Context(), me, sessionToken(r)); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/admin/users")
}

func userError(err error) (string, bool) {
	for _, known := range []error{auth.ErrForbidden, auth.ErrUserNotFound, auth.ErrEmailTaken, auth.ErrInvalidEmail,
		auth.ErrDomainNotAllowed} {
		if errors.Is(err, known) {
			return err.Error(), true
		}
	}
	return "", false
}
