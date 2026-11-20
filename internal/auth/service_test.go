package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/settings"
	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

const testPassword = "the-right-password"

func newTestService(t *testing.T) (*Service, *pgxpool.Pool, int64) {
	t.Helper()
	pool := storetest.New(t)
	st := settings.New(pool)
	if err := st.SetAuth(context.Background(), settings.Auth{AllowedEmailDomains: []string{"example.com"}}, audit.Actor{Label: "test"}); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(pool, st, filepath.Join(t.TempDir(), "master-password"), discard)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	err = pool.QueryRow(context.Background(), `
		INSERT INTO users (auth_mode, email, nickname, password_hash) VALUES ('local', 'alice@example.com', 'alice', $1)
		RETURNING id`, hash).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return svc, pool, id
}

func TestLoginAndAuthenticate(t *testing.T) {
	svc, _, id := newTestService(t)
	ctx := context.Background()

	session, err := svc.Login(ctx, "  Alice@Example.com ", testPassword, "10.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if until := time.Until(session.ExpiresAt); until < 11*time.Hour || until > 13*time.Hour {
		t.Errorf("session expires in %v, want about 12h", until)
	}

	user, err := svc.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user.ID != id || user.Email != "alice@example.com" || user.IsAdmin() {
		t.Errorf("user = %+v", user)
	}

	if err := svc.Logout(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, session.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("Authenticate after logout: err = %v, want ErrNoSession", err)
	}
}

func TestLoginFailures(t *testing.T) {
	svc, pool, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Login(ctx, "nobody@example.com", testPassword, "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("unknown account: err = %v", err)
	}

	for i := range 5 {
		if _, err := svc.Login(ctx, "alice@example.com", "wrong", "", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d: err = %v", i+1, err)
		}
	}
	if _, err := svc.Login(ctx, "alice@example.com", testPassword, "", ""); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("right password during the wait: err = %v, want ErrTooManyAttempts", err)
	}

	storetest.MustExec(t, pool, "UPDATE users SET locked_until = now() - interval '1 second'")
	if _, err := svc.Login(ctx, "alice@example.com", testPassword, "", ""); err != nil {
		t.Fatalf("login after the wait: %v", err)
	}

	var failures int
	if err := pool.QueryRow(ctx, "SELECT failed_logins FROM users WHERE email = 'alice@example.com'").Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 0 {
		t.Errorf("failed_logins = %d after a successful login, want 0", failures)
	}
}

func TestSessionRejected(t *testing.T) {
	svc, pool, _ := newTestService(t)
	ctx := context.Background()

	session, err := svc.Login(ctx, "alice@example.com", testPassword, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, "not-a-real-token"); !errors.Is(err, ErrNoSession) {
		t.Errorf("unknown token: err = %v", err)
	}

	storetest.MustExec(t, pool, "UPDATE users SET banned_at = now()")
	if _, err := svc.Authenticate(ctx, session.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("banned user: err = %v", err)
	}
	if _, err := svc.Login(ctx, "alice@example.com", testPassword, "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("banned user login: err = %v", err)
	}

	storetest.MustExec(t, pool, "UPDATE users SET banned_at = NULL")
	storetest.MustExec(t, pool, "UPDATE sessions SET expires_at = now() - interval '1 second'")
	if _, err := svc.Authenticate(ctx, session.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("expired session: err = %v", err)
	}
}

func TestMasterLoginRemovesPasswordFile(t *testing.T) {
	pool := storetest.New(t)
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "master-password")
	if err := EnsureMaster(ctx, pool, file, discard); err != nil {
		t.Fatal(err)
	}
	password, err := ReadMasterPassword(file)
	if err != nil {
		t.Fatal(err)
	}

	svc, err := NewService(pool, settings.New(pool), file, discard)
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.Login(ctx, "admin", password[:len(password)-1], "", "")
	if err != nil {
		t.Fatalf("master login: %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("password file still exists after the master's first login (err=%v)", err)
	}
	user, err := svc.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !user.IsMaster || !user.IsAdmin() || user.Label() != "admin" {
		t.Errorf("master user = %+v", user)
	}
}

func TestLoginAudited(t *testing.T) {
	svc, pool, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.Login(ctx, "alice@example.com", "wrong", "", "")
	_, _ = svc.Login(ctx, "alice@example.com", testPassword, "", "")

	rows, err := pool.Query(ctx, "SELECT action FROM audit_log WHERE action LIKE 'login.%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a)
	}
	if len(actions) != 2 || actions[0] != "login.failure" || actions[1] != "login.success" {
		t.Errorf("audited actions = %v", actions)
	}
}
