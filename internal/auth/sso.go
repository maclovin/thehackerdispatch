package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

var (
	ErrEmailNotVerified = errors.New("your email isn't verified in the company's identity provider")
	ErrLocalAccount     = errors.New("this email signs in with a password, not with SSO")
	ErrBanned           = errors.New("this account is blocked")
)

type SSO struct {
	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func NewSSO(issuer, clientID, clientSecret, redirectURL string) *SSO {
	return &SSO{issuer: issuer, clientID: clientID, clientSecret: clientSecret, redirectURL: redirectURL}
}

func (s *SSO) client(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oauth != nil {
		return s.oauth, s.verifier, nil
	}
	provider, err := oidc.NewProvider(ctx, s.issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("discover the identity provider: %w", err)
	}
	s.oauth = &oauth2.Config{
		ClientID:     s.clientID,
		ClientSecret: s.clientSecret,
		RedirectURL:  s.redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	s.verifier = provider.Verifier(&oidc.Config{ClientID: s.clientID})
	return s.oauth, s.verifier, nil
}

func (s *SSO) AuthCodeURL(ctx context.Context, state, nonce, pkceVerifier string) (string, error) {
	cfg, _, err := s.client(ctx)
	if err != nil {
		return "", err
	}
	return cfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkceVerifier)), nil
}

type Claims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

func (s *SSO) Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Claims, error) {
	cfg, verifier, err := s.client(ctx)
	if err != nil {
		return Claims{}, err
	}
	token, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Claims{}, fmt.Errorf("exchange the code: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return Claims{}, errors.New("the identity provider returned no ID token")
	}
	idToken, err := verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, fmt.Errorf("verify the ID token: %w", err)
	}
	if idToken.Nonce != nonce {
		return Claims{}, errors.New("the ID token's nonce doesn't match")
	}
	var c Claims
	if err := idToken.Claims(&c); err != nil {
		return Claims{}, err
	}
	c.Subject = idToken.Subject
	return c, nil
}

func (s *Service) LoginSSO(ctx context.Context, c Claims, ip, userAgent string) (Session, error) {
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if !c.EmailVerified || email == "" {
		s.recordLogin(ctx, 0, email, "login.failure", "SSO email not verified")
		return Session{}, ErrEmailNotVerified
	}
	cfg, err := s.settings.Auth(ctx)
	if err != nil {
		return Session{}, err
	}
	if !cfg.EmailAllowed(email) {
		s.recordLogin(ctx, 0, email, "login.failure", "SSO email domain not allowed")
		return Session{}, ErrDomainNotAllowed
	}

	var id int64
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			mode   string
			banned bool
		)
		err := tx.QueryRow(ctx, `
			SELECT id, auth_mode, banned_at IS NOT NULL FROM users
			WHERE sso_subject = $1 OR (email = $2 AND sso_subject IS NULL)
			ORDER BY sso_subject IS NULL LIMIT 1`, c.Subject, email).Scan(&id, &mode, &banned)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			err = tx.QueryRow(ctx, `
				INSERT INTO users (auth_mode, email, sso_subject, real_name) VALUES ('sso', $1, $2, $3) RETURNING id`,
				email, c.Subject, truncate(c.Name, 100)).Scan(&id)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{ActorID: id, ActorLabel: email, Action: "user.created_by_sso", Target: userTarget(id)})
		case err != nil:
			return err
		case banned:
			return ErrBanned
		case mode != "sso":
			return ErrLocalAccount
		}
		_, err = tx.Exec(ctx, `
			UPDATE users SET sso_subject = $2, email = $3, real_name = CASE WHEN $4 = '' THEN real_name ELSE $4 END
			WHERE id = $1`, id, c.Subject, email, truncate(c.Name, 100))
		if isUnique(err) {
			return ErrEmailTaken
		}
		return err
	})
	if err != nil {
		s.recordLogin(ctx, id, email, "login.failure", err.Error())
		return Session{}, err
	}
	if _, err := s.pool.Exec(ctx, "UPDATE users SET last_login_at = now() WHERE id = $1", id); err != nil {
		return Session{}, err
	}
	session, err := s.createSession(ctx, id, ip, userAgent, cfg.SessionLifetime())
	if err != nil {
		return Session{}, err
	}
	s.recordLogin(ctx, id, email, "login.success", "sso")
	return session, nil
}
