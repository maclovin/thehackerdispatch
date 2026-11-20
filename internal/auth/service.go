package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/settings"
)

var (
	ErrInvalidCredentials  = errors.New("invalid email or password")
	ErrTooManyAttempts     = errors.New("too many failed attempts")
	ErrBusy                = errors.New("too many logins in progress")
	ErrNoSession           = errors.New("no valid session")
	ErrDomainNotAllowed    = errors.New("this email's domain is not allowed to sign in")
	ErrTempPasswordExpired = errors.New("the temporary password has expired")
)

const maxConcurrentVerifications = 4

const lastSeenResolution = time.Minute

type User struct {
	ID                int64
	Email             string
	Nickname          string
	RealName          string
	Role              string
	Theme             string
	AuthMode          string
	IsMaster          bool
	MustRotate        bool
	PreferredSections []string
}

func (u User) IsAdmin() bool { return u.Role == "admin" }

func (u User) Label() string {
	if u.IsMaster {
		return MasterNickname
	}
	return u.Email
}

func (u User) Actor() audit.Actor { return audit.Actor{ID: u.ID, Label: u.Label()} }

type Session struct {
	Token     string
	ExpiresAt time.Time
}

type Service struct {
	pool               *pgxpool.Pool
	settings           *settings.Store
	masterPasswordFile string
	logger             *slog.Logger
	verifySlots        chan struct{}
	dummyHash          string
}

func NewService(pool *pgxpool.Pool, settings *settings.Store, masterPasswordFile string, logger *slog.Logger) (*Service, error) {
	// Verified when the account doesn't exist, so timing doesn't reveal which accounts exist.
	dummy, err := HashPassword("dummy password")
	if err != nil {
		return nil, err
	}
	return &Service{
		pool:               pool,
		settings:           settings,
		masterPasswordFile: masterPasswordFile,
		logger:             logger,
		verifySlots:        make(chan struct{}, maxConcurrentVerifications),
		dummyHash:          dummy,
	}, nil
}

func (s *Service) Login(ctx context.Context, login, password, ip, userAgent string) (Session, error) {
	login = strings.ToLower(strings.TrimSpace(login))

	select {
	case s.verifySlots <- struct{}{}:
		defer func() { <-s.verifySlots }()
	case <-time.After(time.Second):
		return Session{}, ErrBusy
	case <-ctx.Done():
		return Session{}, ctx.Err()
	}

	cfg, err := s.settings.Auth(ctx)
	if err != nil {
		s.logger.Error("load auth settings, using defaults", "err", err)
	}

	var (
		id          int64
		email       string
		hash        string
		lockedUntil *time.Time
		tempExpires *time.Time
		mustRotate  bool
		isMaster    bool
	)
	err = s.pool.QueryRow(ctx, `
		SELECT id, coalesce(email, ''), password_hash, locked_until, password_temp_expires_at, must_rotate, is_master
		FROM users
		WHERE auth_mode = 'local' AND banned_at IS NULL AND password_hash IS NOT NULL
		  AND ((is_master AND $1 = $2) OR email = $1)`,
		login, MasterNickname).Scan(&id, &email, &hash, &lockedUntil, &tempExpires, &mustRotate, &isMaster)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = VerifyPassword(password, s.dummyHash)
		s.recordLogin(ctx, 0, login, "login.failure", "unknown account")
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, fmt.Errorf("load account: %w", err)
	}

	if lockedUntil != nil && time.Now().Before(*lockedUntil) {
		s.recordLogin(ctx, id, login, "login.failure", "waiting after failed attempts")
		return Session{}, ErrTooManyAttempts
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil {
		return Session{}, fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		var failures int
		err := s.pool.QueryRow(ctx,
			"UPDATE users SET failed_logins = failed_logins + 1 WHERE id = $1 RETURNING failed_logins", id).Scan(&failures)
		if err != nil {
			return Session{}, fmt.Errorf("count failed login: %w", err)
		}
		if failures >= cfg.MaxFailedLogins {
			if _, err := s.pool.Exec(ctx, "UPDATE users SET failed_logins = 0, locked_until = $2 WHERE id = $1",
				id, time.Now().Add(cfg.LoginWait())); err != nil {
				return Session{}, fmt.Errorf("start login wait: %w", err)
			}
		}
		s.recordLogin(ctx, id, login, "login.failure", "wrong password")
		return Session{}, ErrInvalidCredentials
	}

	// Checked after the password, so they don't reveal which accounts exist.
	if !isMaster && !cfg.EmailAllowed(email) {
		s.recordLogin(ctx, id, login, "login.failure", "email domain not allowed")
		return Session{}, ErrDomainNotAllowed
	}
	if mustRotate && tempExpires != nil && time.Now().After(*tempExpires) {
		s.recordLogin(ctx, id, login, "login.failure", "temporary password expired")
		return Session{}, ErrTempPasswordExpired
	}

	if _, err := s.pool.Exec(ctx,
		"UPDATE users SET failed_logins = 0, locked_until = NULL, last_login_at = now() WHERE id = $1", id); err != nil {
		return Session{}, fmt.Errorf("record login: %w", err)
	}
	session, err := s.createSession(ctx, id, ip, userAgent, cfg.SessionLifetime())
	if err != nil {
		return Session{}, err
	}
	if isMaster {
		if err := os.Remove(s.masterPasswordFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("remove master password file", "err", err)
		}
	}
	s.recordLogin(ctx, id, login, "login.success", "")
	return session, nil
}

func (s *Service) recordLogin(ctx context.Context, userID int64, login, action, reason string) {
	var details map[string]any
	if reason != "" {
		details = map[string]any{"reason": reason}
	}
	err := audit.Record(ctx, s.pool, audit.Entry{
		ActorID: userID, ActorLabel: login, Action: action, Target: login, Details: details,
	})
	if err != nil {
		s.logger.Error("audit login", "err", err)
	}
}

func (s *Service) createSession(ctx context.Context, userID int64, ip, userAgent string, lifetime time.Duration) (Session, error) {
	token := rand.Text() + rand.Text()
	expires := time.Now().Add(lifetime)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id_hash, user_id, ip, user_agent, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		hashToken(token), userID, ip, truncate(userAgent, 300), expires)
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return Session{Token: token, ExpiresAt: expires}, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNoSession
	}
	var (
		u        User
		lastSeen time.Time
	)
	id := hashToken(token)
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, coalesce(u.email, ''), coalesce(u.nickname, ''), u.real_name, u.role, u.theme, u.auth_mode,
		       u.is_master, u.must_rotate, u.preferred_sections, s.last_seen_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id_hash = $1 AND s.expires_at > now() AND u.banned_at IS NULL`, id).
		Scan(&u.ID, &u.Email, &u.Nickname, &u.RealName, &u.Role, &u.Theme, &u.AuthMode, &u.IsMaster, &u.MustRotate,
			&u.PreferredSections, &lastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, fmt.Errorf("load session: %w", err)
	}
	if time.Since(lastSeen) > lastSeenResolution {
		if _, err := s.pool.Exec(ctx, "UPDATE sessions SET last_seen_at = now() WHERE id_hash = $1", id); err != nil {
			s.logger.Warn("update session last_seen_at", "err", err)
		}
	}
	return u, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE id_hash = $1", hashToken(token)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
