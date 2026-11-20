package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"

	"github.com/maclovin/thehackerdispatch/internal/auth"
)

const (
	ssoCookie   = "hd_sso"
	ssoLifetime = 10 * time.Minute
)

var ssoErrors = map[string]string{
	"sso_failed":     "Signing in with SSO didn't work. Try again.",
	"sso_domain":     "Only company email addresses can sign in.",
	"sso_unverified": "Your email isn't verified in the company's identity provider.",
	"sso_local":      "This email signs in with a password, not with SSO.",
	"sso_banned":     "This account is blocked.",
}

type ssoState struct {
	State    string    `json:"s"`
	Nonce    string    `json:"n"`
	Verifier string    `json:"v"`
	Next     string    `json:"x"`
	Expires  time.Time `json:"e"`
}

func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	if s.SSO == nil {
		http.NotFound(w, r)
		return
	}
	st := ssoState{
		State:    oauth2.GenerateVerifier(),
		Nonce:    oauth2.GenerateVerifier(),
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
		Expires:  time.Now().Add(ssoLifetime),
	}
	target, err := s.SSO.AuthCodeURL(r.Context(), st.State, st.Nonce, st.Verifier)
	if err != nil {
		s.Logger.Error("start SSO", "err", err)
		http.Redirect(w, r, "/login?error=sso_failed", http.StatusSeeOther)
		return
	}
	raw, _ := json.Marshal(st)
	http.SetCookie(w, &http.Cookie{
		Name:     ssoCookie,
		Value:    base64.RawURLEncoding.EncodeToString(s.Box.Seal(raw)),
		Path:     "/auth/",
		MaxAge:   int(ssoLifetime.Seconds()),
		HttpOnly: true,
		Secure:   s.BaseURL != nil && s.BaseURL.Scheme == "https",
		// Lax: the cookie must come back on the top-level redirect from Keycloak.
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	if s.SSO == nil {
		http.NotFound(w, r)
		return
	}
	fail := func(code string, err error) {
		s.Logger.Warn("SSO sign-in failed", "reason", code, "err", err)
		http.Redirect(w, r, "/login?error="+url.QueryEscape(code), http.StatusSeeOther)
	}
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Path: "/auth/", MaxAge: -1})

	c, err := r.Cookie(ssoCookie)
	if err != nil {
		fail("sso_failed", errors.New("no SSO state cookie"))
		return
	}
	sealed, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		fail("sso_failed", err)
		return
	}
	raw, err := s.Box.Open(sealed)
	if err != nil {
		fail("sso_failed", err)
		return
	}
	var st ssoState
	if err := json.Unmarshal(raw, &st); err != nil || time.Now().After(st.Expires) || r.URL.Query().Get("state") != st.State {
		fail("sso_failed", errors.New("state missing, expired or mismatched"))
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		fail("sso_failed", errors.New("identity provider returned "+e))
		return
	}

	claims, err := s.SSO.Exchange(r.Context(), r.URL.Query().Get("code"), st.Verifier, st.Nonce)
	if err != nil {
		fail("sso_failed", err)
		return
	}
	session, err := s.Auth.LoginSSO(r.Context(), claims, clientIP(r), r.UserAgent())
	switch {
	case errors.Is(err, auth.ErrDomainNotAllowed):
		fail("sso_domain", err)
	case errors.Is(err, auth.ErrEmailNotVerified):
		fail("sso_unverified", err)
	case errors.Is(err, auth.ErrLocalAccount):
		fail("sso_local", err)
	case errors.Is(err, auth.ErrBanned):
		fail("sso_banned", err)
	case err != nil:
		fail("sso_failed", err)
	default:
		s.setSessionCookie(w, session.Token, session.ExpiresAt)
		http.Redirect(w, r, st.Next, http.StatusSeeOther)
	}
}
