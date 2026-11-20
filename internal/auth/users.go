package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

var (
	ErrUserNotFound    = errors.New("user not found")
	ErrForbidden       = errors.New("not allowed")
	ErrEmailTaken      = errors.New("an account with this email already exists")
	ErrInvalidEmail    = errors.New("enter a valid email address")
	ErrNicknameTaken   = errors.New("this nickname is taken")
	ErrInvalidNickname = errors.New("nicknames have 3 to 24 characters: lowercase letters, digits, - and _")
	ErrWrongPassword   = errors.New("the current password is wrong")
)

const tempPasswordLifetime = 72 * time.Hour

var (
	nicknameRe        = regexp.MustCompile(`^[a-z0-9_-]{3,24}$`)
	reservedNicknames = []string{MasterNickname, "root", "system", "deleted", "anonymous", "moderator", "mod", "staff", "support", "null"}
)

type Account struct {
	ID          int64
	Email       string
	Nickname    string
	RealName    string
	Role        string
	AuthMode    string
	IsMaster    bool
	Banned      bool
	MustRotate  bool
	LastLoginAt *time.Time
	CreatedAt   time.Time
	Sessions    int
}

const accountColumns = `u.id, coalesce(u.email, ''), coalesce(u.nickname, ''), u.real_name, u.role, u.auth_mode,
	u.is_master, u.banned_at IS NOT NULL, u.must_rotate, u.last_login_at, u.created_at,
	(SELECT count(*) FROM sessions s WHERE s.user_id = u.id AND s.expires_at > now())`

func (s *Service) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accountColumns+` FROM users u ORDER BY u.is_master DESC, lower(coalesce(u.nickname, u.email))`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Account])
}

func (s *Service) Account(ctx context.Context, id int64) (Account, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accountColumns+` FROM users u WHERE u.id = $1`, id)
	if err != nil {
		return Account{}, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Account])
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrUserNotFound
	}
	return a, err
}

type NewAccount struct {
	Email    string
	RealName string
	Role     string
	AuthMode string
}

func (s *Service) CreateAccount(ctx context.Context, actor User, in NewAccount) (int64, string, error) {
	if !actor.IsAdmin() {
		return 0, "", ErrForbidden
	}
	email, err := s.checkEmail(ctx, in.Email)
	if err != nil {
		return 0, "", err
	}
	if in.Role != "admin" {
		in.Role = "member"
	}
	if in.AuthMode != "sso" {
		in.AuthMode = "local"
	}
	in.RealName = truncate(strings.TrimSpace(in.RealName), 100)

	var (
		id       int64
		password string
		hash     *string
		expires  *time.Time
	)
	if in.AuthMode == "local" {
		if password, err = GeneratePassword(); err != nil {
			return 0, "", err
		}
		h, err := HashPassword(password)
		if err != nil {
			return 0, "", err
		}
		e := time.Now().Add(tempPasswordLifetime)
		hash, expires = &h, &e
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO users (auth_mode, email, real_name, role, password_hash, password_temp_expires_at, must_rotate)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			in.AuthMode, email, in.RealName, in.Role, hash, expires, in.AuthMode == "local").Scan(&id)
		if isUnique(err) {
			return ErrEmailTaken
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Actor().Entry("user.created", userTarget(id),
			map[string]any{"email": email, "role": in.Role, "auth_mode": in.AuthMode}))
	})
	if err != nil {
		return 0, "", err
	}
	return id, password, nil
}

func (s *Service) ResetPassword(ctx context.Context, actor User, targetID int64) (string, error) {
	target, err := s.manageable(ctx, actor, targetID, true)
	if err != nil {
		return "", err
	}
	if target.AuthMode != "local" {
		return "", fmt.Errorf("%w: this account signs in with SSO", ErrForbidden)
	}
	return s.issueTempPassword(ctx, actor, targetID, "user.password_reset")
}

func (s *Service) SetAuthMode(ctx context.Context, actor User, targetID int64, mode string) (string, error) {
	target, err := s.manageable(ctx, actor, targetID, false)
	if err != nil {
		return "", err
	}
	if mode != "sso" && mode != "local" {
		return "", fmt.Errorf("%w: unknown login mode", ErrForbidden)
	}
	if mode == target.AuthMode {
		return "", nil
	}
	if mode == "local" {
		if _, err := s.pool.Exec(ctx, "UPDATE users SET auth_mode = 'local', sso_subject = NULL WHERE id = $1", targetID); err != nil {
			return "", err
		}
		return s.issueTempPassword(ctx, actor, targetID, "user.mode_local")
	}
	return "", pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET auth_mode = 'sso', password_hash = NULL, password_temp_expires_at = NULL, must_rotate = false
			WHERE id = $1`, targetID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1", targetID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Actor().Entry("user.mode_sso", userTarget(targetID), nil))
	})
}

func (s *Service) SetRole(ctx context.Context, actor User, targetID int64, role string) error {
	if _, err := s.manageable(ctx, actor, targetID, false); err != nil {
		return err
	}
	if role != "admin" {
		role = "member"
	}
	return s.changeAndRevoke(ctx, actor, targetID, "user.role_"+role, "UPDATE users SET role = $2 WHERE id = $1", role)
}

func (s *Service) SetBanned(ctx context.Context, actor User, targetID int64, banned bool) error {
	if _, err := s.manageable(ctx, actor, targetID, false); err != nil {
		return err
	}
	if banned {
		return s.changeAndRevoke(ctx, actor, targetID, "user.banned", "UPDATE users SET banned_at = now() WHERE id = $1")
	}
	return s.changeAndRevoke(ctx, actor, targetID, "user.unbanned", "UPDATE users SET banned_at = NULL WHERE id = $1")
}

func (s *Service) DeleteUser(ctx context.Context, actor User, targetID int64) error {
	target, err := s.manageable(ctx, actor, targetID, false)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE comments SET deleted = true, body_md = '' WHERE user_id = $1", targetID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM users WHERE id = $1", targetID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Actor().Entry("user.deleted", userTarget(targetID),
			map[string]any{"email": target.Email, "nickname": target.Nickname}))
	})
}

func (s *Service) manageable(ctx context.Context, actor User, targetID int64, allowMaster bool) (Account, error) {
	if !actor.IsAdmin() {
		return Account{}, ErrForbidden
	}
	if actor.ID == targetID {
		return Account{}, fmt.Errorf("%w: use your own account page instead", ErrForbidden)
	}
	target, err := s.Account(ctx, targetID)
	if err != nil {
		return Account{}, err
	}
	if target.IsMaster && !allowMaster {
		return Account{}, fmt.Errorf("%w: the master account can't be changed", ErrForbidden)
	}
	return target, nil
}

func (s *Service) issueTempPassword(ctx context.Context, actor User, targetID int64, action string) (string, error) {
	password, err := GeneratePassword()
	if err != nil {
		return "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	err = s.changeAndRevoke(ctx, actor, targetID, action, `
		UPDATE users SET password_hash = $2, password_temp_expires_at = $3, must_rotate = true,
			failed_logins = 0, locked_until = NULL
		WHERE id = $1`, hash, time.Now().Add(tempPasswordLifetime))
	if err != nil {
		return "", err
	}
	return password, nil
}

func (s *Service) changeAndRevoke(ctx context.Context, actor User, targetID int64, action, sql string, args ...any) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, append([]any{targetID}, args...)...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrUserNotFound
		}
		if _, err := tx.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1", targetID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Actor().Entry(action, userTarget(targetID), nil))
	})
}

func (s *Service) checkEmail(ctx context.Context, raw string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil || addr.Name != "" {
		return "", ErrInvalidEmail
	}
	email := strings.ToLower(addr.Address)
	cfg, err := s.settings.Auth(ctx)
	if err != nil {
		return "", err
	}
	if !cfg.EmailAllowed(email) {
		return "", fmt.Errorf("%w (allowed: %s)", ErrDomainNotAllowed, strings.Join(cfg.AllowedEmailDomains, ", "))
	}
	return email, nil
}

func (s *Service) RotatePassword(ctx context.Context, user User, current, keepToken string) (string, error) {
	var (
		hash       *string
		mustRotate bool
	)
	if err := s.pool.QueryRow(ctx, "SELECT password_hash, must_rotate FROM users WHERE id = $1 AND auth_mode = 'local'",
		user.ID).Scan(&hash, &mustRotate); err != nil || hash == nil {
		return "", ErrForbidden
	}
	if !mustRotate {
		ok, err := VerifyPassword(current, *hash)
		if err != nil || !ok {
			return "", ErrWrongPassword
		}
	}
	password, err := GeneratePassword()
	if err != nil {
		return "", err
	}
	newHash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET password_hash = $2, must_rotate = false, password_temp_expires_at = NULL WHERE id = $1`,
			user.ID, newHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1 AND id_hash <> $2", user.ID, hashToken(keepToken)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, user.Actor().Entry("user.password_changed", userTarget(user.ID), nil))
	})
	if err != nil {
		return "", err
	}
	return password, nil
}

func (s *Service) SetNickname(ctx context.Context, user User, nickname string) error {
	nickname = strings.ToLower(strings.TrimSpace(nickname))
	if !nicknameRe.MatchString(nickname) || (slices.Contains(reservedNicknames, nickname) && !user.IsMaster) {
		return ErrInvalidNickname
	}
	_, err := s.pool.Exec(ctx, "UPDATE users SET nickname = $2 WHERE id = $1", user.ID, nickname)
	if isUnique(err) {
		return ErrNicknameTaken
	}
	return err
}

func (s *Service) SetTheme(ctx context.Context, user User, theme string) error {
	if theme != "light" && theme != "dark" {
		theme = "system"
	}
	_, err := s.pool.Exec(ctx, "UPDATE users SET theme = $2 WHERE id = $1", user.ID, theme)
	return err
}

func (s *Service) SetPreferredSections(ctx context.Context, user User, slugs []string) error {
	if slugs == nil {
		slugs = []string{}
	}
	_, err := s.pool.Exec(ctx, "UPDATE users SET preferred_sections = $2 WHERE id = $1", user.ID, slugs)
	return err
}

// SessionInfo.ID is the hex of the stored hash, never the token.
type SessionInfo struct {
	ID         string
	UserID     int64
	User       string
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	Current    bool
}

func (s *Service) Sessions(ctx context.Context, userID int64, currentToken string) ([]SessionInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id_hash, s.user_id, coalesce(u.nickname, u.email, ''), s.ip, s.user_agent, s.created_at, s.last_seen_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.expires_at > now() AND ($1 = 0 OR s.user_id = $1)
		ORDER BY s.last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	current := hex.EncodeToString(hashToken(currentToken))
	var out []SessionInfo
	var idHash []byte
	var si SessionInfo
	_, err = pgx.ForEachRow(rows, []any{&idHash, &si.UserID, &si.User, &si.IP, &si.UserAgent, &si.CreatedAt, &si.LastSeenAt}, func() error {
		si.ID = hex.EncodeToString(idHash)
		si.Current = si.ID == current
		out = append(out, si)
		return nil
	})
	return out, err
}

func (s *Service) EndSession(ctx context.Context, actor User, sessionID string) error {
	idHash, err := hex.DecodeString(sessionID)
	if err != nil {
		return ErrNoSession
	}
	tag, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE id_hash = $1 AND (user_id = $2 OR $3)", idHash, actor.ID, actor.IsAdmin())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSession
	}
	return audit.Record(ctx, s.pool, actor.Actor().Entry("session.ended", "session:"+sessionID[:12], nil))
}

func (s *Service) EndOtherSessions(ctx context.Context, user User, keepToken string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1 AND id_hash <> $2", user.ID, hashToken(keepToken))
	return err
}

func (s *Service) EndAllSessions(ctx context.Context, actor User, keepToken string) error {
	if !actor.IsAdmin() {
		return ErrForbidden
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE id_hash <> $1", hashToken(keepToken)); err != nil {
		return err
	}
	return audit.Record(ctx, s.pool, actor.Actor().Entry("session.ended_all", "sessions", nil))
}

func userTarget(id int64) string { return fmt.Sprintf("user:%d", id) }

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
