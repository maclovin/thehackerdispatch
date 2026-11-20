package settings

import (
	"context"
	"strings"
	"time"
)

type Auth struct {
	SessionLifetimeHours int      `json:"session_lifetime_hours"`
	MaxFailedLogins      int      `json:"max_failed_logins"`
	LoginWaitMinutes     int      `json:"login_wait_minutes"`
	AllowedEmailDomains  []string `json:"allowed_email_domains"`
}

func DefaultAuth() Auth {
	return Auth{SessionLifetimeHours: 12, MaxFailedLogins: 5, LoginWaitMinutes: 15}
}

func (a Auth) EmailAllowed(email string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 1 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, d := range a.AllowedEmailDomains {
		if strings.EqualFold(strings.TrimSpace(d), domain) {
			return true
		}
	}
	return false
}

func ParseDomains(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		if d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(f), "@")); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func (a Auth) SessionLifetime() time.Duration {
	return time.Duration(a.SessionLifetimeHours) * time.Hour
}

func (a Auth) LoginWait() time.Duration {
	return time.Duration(a.LoginWaitMinutes) * time.Minute
}

func (s *Store) Auth(ctx context.Context) (Auth, error) {
	a := DefaultAuth()
	if err := s.get(ctx, "auth", &a); err != nil {
		return DefaultAuth(), err
	}
	def := DefaultAuth()
	if a.SessionLifetimeHours < 1 || a.SessionLifetimeHours > 24*30 {
		a.SessionLifetimeHours = def.SessionLifetimeHours
	}
	if a.MaxFailedLogins < 1 {
		a.MaxFailedLogins = def.MaxFailedLogins
	}
	if a.LoginWaitMinutes < 1 {
		a.LoginWaitMinutes = def.LoginWaitMinutes
	}
	return a, nil
}
