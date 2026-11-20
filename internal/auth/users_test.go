package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/settings"
)

func addAdmin(t *testing.T, svc *Service) User {
	t.Helper()
	var id int64
	err := svc.pool.QueryRow(context.Background(), `
		INSERT INTO users (auth_mode, email, nickname, role) VALUES ('sso', 'boss@example.com', 'boss', 'admin') RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return User{ID: id, Role: "admin", Email: "boss@example.com"}
}

func TestLocalAccountLifecycle(t *testing.T) {
	svc, _, aliceID := newTestService(t)
	ctx := context.Background()
	admin := User{ID: aliceID, Role: "admin", Email: "alice@example.com"}

	if _, _, err := svc.CreateAccount(ctx, User{ID: aliceID, Role: "member"}, NewAccount{Email: "dan@example.com"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("member creating an account: err = %v", err)
	}
	if _, _, err := svc.CreateAccount(ctx, admin, NewAccount{Email: "dan@evil.com"}); !errors.Is(err, ErrDomainNotAllowed) {
		t.Errorf("foreign domain: err = %v", err)
	}
	if _, _, err := svc.CreateAccount(ctx, admin, NewAccount{Email: "not an email"}); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("invalid email: err = %v", err)
	}
	id, temp, err := svc.CreateAccount(ctx, admin, NewAccount{Email: "Dan@Example.com", RealName: "Dan"})
	if err != nil || len(temp) != passwordLength {
		t.Fatalf("CreateAccount: %v (password %q)", err, temp)
	}
	if _, _, err := svc.CreateAccount(ctx, admin, NewAccount{Email: "dan@example.com"}); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("duplicate email: err = %v", err)
	}

	first, err := svc.Login(ctx, "dan@example.com", temp, "", "")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := svc.Login(ctx, "dan@example.com", temp, "", "")
	dan, _ := svc.Authenticate(ctx, first.Token)
	if !dan.MustRotate || dan.ID != id {
		t.Fatalf("dan = %+v", dan)
	}
	fresh, err := svc.RotatePassword(ctx, dan, "", first.Token)
	if err != nil || fresh == temp {
		t.Fatalf("RotatePassword: %v", err)
	}
	if _, err := svc.Authenticate(ctx, other.Token); !errors.Is(err, ErrNoSession) {
		t.Error("rotating the password should end the other sessions")
	}
	if _, err := svc.Authenticate(ctx, first.Token); err != nil {
		t.Error("rotating the password should keep the current session")
	}
	if _, err := svc.Login(ctx, "dan@example.com", temp, "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("old temporary password still works: err = %v", err)
	}

	dan, _ = svc.Authenticate(ctx, first.Token)
	if _, err := svc.RotatePassword(ctx, dan, "wrong", first.Token); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("rotate with a wrong password: err = %v", err)
	}
	if _, err := svc.RotatePassword(ctx, dan, fresh, first.Token); err != nil {
		t.Errorf("rotate with the right password: %v", err)
	}

	reset, err := svc.ResetPassword(ctx, admin, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.pool.Exec(ctx, "UPDATE users SET password_temp_expires_at = now() - interval '1 minute' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "dan@example.com", reset, "", ""); !errors.Is(err, ErrTempPasswordExpired) {
		t.Errorf("expired temporary password: err = %v", err)
	}

	if err := svc.settings.SetAuth(ctx, settings.Auth{AllowedEmailDomains: []string{"other.org"}}, audit.Actor{Label: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "alice@example.com", testPassword, "", ""); !errors.Is(err, ErrDomainNotAllowed) {
		t.Errorf("login after the domain was removed: err = %v", err)
	}
}

func TestAdminRules(t *testing.T) {
	svc, pool, aliceID := newTestService(t)
	ctx := context.Background()
	if err := EnsureMaster(ctx, pool, t.TempDir()+"/pw", discard); err != nil {
		t.Fatal(err)
	}
	var masterID int64
	if err := pool.QueryRow(ctx, "SELECT id FROM users WHERE is_master").Scan(&masterID); err != nil {
		t.Fatal(err)
	}
	master := User{ID: masterID, Role: "admin", IsMaster: true}
	alice := User{ID: aliceID, Role: "admin", Email: "alice@example.com"}
	if _, err := pool.Exec(ctx, "UPDATE users SET role = 'admin' WHERE id = $1", aliceID); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.ResetPassword(ctx, alice, aliceID); !errors.Is(err, ErrForbidden) {
		t.Errorf("reset own password: err = %v", err)
	}
	if err := svc.SetRole(ctx, alice, aliceID, "member"); !errors.Is(err, ErrForbidden) {
		t.Errorf("demote self: err = %v", err)
	}
	if err := svc.SetBanned(ctx, alice, masterID, true); !errors.Is(err, ErrForbidden) {
		t.Errorf("ban master: err = %v", err)
	}
	if err := svc.DeleteUser(ctx, alice, masterID); !errors.Is(err, ErrForbidden) {
		t.Errorf("delete master: err = %v", err)
	}
	if _, err := svc.SetAuthMode(ctx, alice, masterID, "sso"); !errors.Is(err, ErrForbidden) {
		t.Errorf("switch master to SSO: err = %v", err)
	}
	newMaster, err := svc.ResetPassword(ctx, alice, masterID)
	if err != nil {
		t.Fatalf("another admin resetting the master's password: %v", err)
	}
	if _, err := svc.Login(ctx, "admin", newMaster, "", ""); err != nil {
		t.Errorf("master login with the reset password: %v", err)
	}

	session, err := svc.Login(ctx, "alice@example.com", testPassword, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetBanned(ctx, master, aliceID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, session.Token); !errors.Is(err, ErrNoSession) {
		t.Error("ban should end sessions")
	}
	if err := svc.SetBanned(ctx, master, aliceID, false); err != nil {
		t.Fatal(err)
	}

	var storyID, commentID int64
	pool.QueryRow(ctx, "INSERT INTO stories (title, section_id) VALUES ('s', 1) RETURNING id").Scan(&storyID)
	pool.QueryRow(ctx, "INSERT INTO comments (story_id, user_id, body_md) VALUES ($1, $2, 'hello') RETURNING id", storyID, aliceID).Scan(&commentID)
	if err := svc.DeleteUser(ctx, master, aliceID); err != nil {
		t.Fatal(err)
	}
	var deleted bool
	var body string
	if err := pool.QueryRow(ctx, "SELECT deleted, body_md FROM comments WHERE id = $1", commentID).Scan(&deleted, &body); err != nil {
		t.Fatalf("comment of a deleted user was removed: %v", err)
	}
	if !deleted || body != "" {
		t.Errorf("comment of a deleted user: deleted=%v body=%q", deleted, body)
	}
}

func TestSwitchAuthMode(t *testing.T) {
	svc, _, aliceID := newTestService(t)
	ctx := context.Background()
	admin := addAdmin(t, svc)

	if _, err := svc.SetAuthMode(ctx, admin, aliceID, "sso"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "alice@example.com", testPassword, "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("password login after switching to SSO: err = %v", err)
	}
	temp, err := svc.SetAuthMode(ctx, admin, aliceID, "local")
	if err != nil || temp == "" {
		t.Fatalf("switch back to local: %v", err)
	}
	if _, err := svc.Login(ctx, "alice@example.com", temp, "", ""); err != nil {
		t.Errorf("login with the new temporary password: %v", err)
	}
}

func TestNicknameAndSessions(t *testing.T) {
	svc, _, aliceID := newTestService(t)
	ctx := context.Background()
	alice := User{ID: aliceID}

	for _, bad := range []string{"ab", "has space", "UPPER!", "admin", "deleted"} {
		if err := svc.SetNickname(ctx, alice, bad); !errors.Is(err, ErrInvalidNickname) {
			t.Errorf("nickname %q: err = %v", bad, err)
		}
	}
	if err := svc.SetNickname(ctx, alice, "Alice_01"); err != nil {
		t.Fatalf("valid nickname: %v", err)
	}
	id, _, err := svc.CreateAccount(ctx, addAdmin(t, svc), NewAccount{Email: "bob@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetNickname(ctx, User{ID: id}, "alice_01"); !errors.Is(err, ErrNicknameTaken) {
		t.Errorf("taken nickname: err = %v", err)
	}

	s1, _ := svc.Login(ctx, "alice@example.com", testPassword, "10.0.0.1", "browser one")
	s2, _ := svc.Login(ctx, "alice@example.com", testPassword, "10.0.0.2", "browser two")
	list, err := svc.Sessions(ctx, aliceID, s1.Token)
	if err != nil || len(list) != 2 {
		t.Fatalf("sessions = %v, err = %v", list, err)
	}
	var other SessionInfo
	for _, si := range list {
		if !si.Current {
			other = si
		}
	}
	if err := svc.EndSession(ctx, User{ID: id}, other.ID); !errors.Is(err, ErrNoSession) {
		t.Errorf("someone else ending alice's session: err = %v", err)
	}
	if err := svc.EndSession(ctx, alice, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, s2.Token); !errors.Is(err, ErrNoSession) {
		t.Error("ended session still works")
	}
	if _, err := svc.Authenticate(ctx, s1.Token); err != nil {
		t.Error("current session was ended")
	}
}
