package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/maclovin/thehackerdispatch/internal/auth"
)

const sessionCookie = "hd_session"

type userKey struct{}

func currentUser(r *http.Request) (auth.User, bool) {
	u, ok := r.Context().Value(userKey{}).(auth.User)
	return u, ok
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			user, err := s.Auth.Authenticate(r.Context(), c.Value)
			if err == nil {
				switch {
				case user.MustRotate && !onAccountPages(r):
					redirect(w, r, "/account/password")
				case user.Nickname == "" && !onAccountPages(r):
					redirect(w, r, "/account")
				default:
					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
				}
				return
			}
			if !errors.Is(err, auth.ErrNoSession) {
				s.Logger.Error("authenticate session", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		s.redirectToLogin(w, r)
	})
}

func onAccountPages(r *http.Request) bool {
	return r.URL.Path == "/logout" || r.URL.Path == "/account" || strings.HasPrefix(r.URL.Path, "/account/")
}

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _ := currentUser(r); !u.IsAdmin() {
			s.renderError(w, r, http.StatusForbidden, "This page is for admins.")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	target := "/login"
	if r.Method == http.MethodGet && r.URL.Path != "/" {
		target += "?next=" + url.QueryEscape(r.URL.RequestURI())
	}
	// htmx would swap the login page into a fragment; ask it to load the whole page instead.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	return next
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.BaseURL != nil && s.BaseURL.Scheme == "https",
		SameSite: http.SameSiteLaxMode,
	})
}

type loginPage struct {
	Login string
	Next  string
	Error string
	SSO   bool
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, err := s.Auth.Authenticate(r.Context(), c.Value); err == nil {
			http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
			return
		}
	}
	// Only known codes: a free-text message in the URL could be used for phishing.
	page := loginPage{Next: safeNext(r.URL.Query().Get("next")), SSO: s.SSO != nil, Error: ssoErrors[r.URL.Query().Get("error")]}
	s.render(w, r, http.StatusOK, "login", "Sign in", page)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	login := r.PostFormValue("login")
	next := safeNext(r.PostFormValue("next"))
	session, err := s.Auth.Login(r.Context(), login, r.PostFormValue("password"), clientIP(r), r.UserAgent())
	if err != nil {
		page := loginPage{Login: login, Next: next, SSO: s.SSO != nil}
		status := http.StatusUnauthorized
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			page.Error = "Invalid email or password."
		case errors.Is(err, auth.ErrDomainNotAllowed):
			status = http.StatusForbidden
			page.Error = "Only company email addresses can sign in."
		case errors.Is(err, auth.ErrTempPasswordExpired):
			page.Error = "Your temporary password has expired. Ask an admin for a new one."
		case errors.Is(err, auth.ErrTooManyAttempts):
			status = http.StatusTooManyRequests
			page.Error = "Too many failed attempts. Wait a few minutes and try again."
		case errors.Is(err, auth.ErrBusy):
			status = http.StatusServiceUnavailable
			page.Error = "Too many sign-ins right now. Try again in a moment."
		default:
			s.Logger.Error("login", "err", err)
			status = http.StatusInternalServerError
			page.Error = "Something went wrong. Try again."
		}
		s.render(w, r, status, "login", "Sign in", page)
		return
	}
	s.setSessionCookie(w, session.Token, session.ExpiresAt)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.Auth.Logout(r.Context(), c.Value); err != nil {
			s.Logger.Error("logout", "err", err)
		}
	}
	s.setSessionCookie(w, "", time.Unix(0, 0))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// clientIP is informational only (session list), so trusting X-Forwarded-For is fine.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		first, _, _ := strings.Cut(fwd, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
