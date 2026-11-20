package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestEnsureMaster(t *testing.T) {
	pool := storetest.New(t)
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "sub", "master-password")

	if err := EnsureMaster(ctx, pool, file, discard); err != nil {
		t.Fatalf("first EnsureMaster: %v", err)
	}

	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("password file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("password file mode = %o, want 600", perm)
	}
	password, err := ReadMasterPassword(file)
	if err != nil {
		t.Fatalf("ReadMasterPassword: %v", err)
	}
	password = strings.TrimSpace(password)

	var hash, role, nickname string
	err = pool.QueryRow(ctx, "SELECT password_hash, role, nickname FROM users WHERE is_master").Scan(&hash, &role, &nickname)
	if err != nil {
		t.Fatalf("select master: %v", err)
	}
	if role != "admin" || nickname != MasterNickname {
		t.Errorf("master role/nickname = %s/%s", role, nickname)
	}
	if ok, err := VerifyPassword(password, hash); err != nil || !ok {
		t.Fatalf("stored hash doesn't match the file's password (ok=%v, err=%v)", ok, err)
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMaster(ctx, pool, file, discard); err != nil {
		t.Fatalf("second EnsureMaster: %v", err)
	}
	if _, err := ReadMasterPassword(file); !errors.Is(err, ErrNoMasterPasswordFile) {
		t.Errorf("second run wrote a password file (err=%v)", err)
	}

	storetest.MustExec(t, pool, "DELETE FROM users WHERE is_master")
	if err := EnsureMaster(ctx, pool, file, discard); err != nil {
		t.Fatalf("EnsureMaster after delete: %v", err)
	}
	if _, err := ReadMasterPassword(file); err != nil {
		t.Errorf("no password file after recovery: %v", err)
	}
}

func TestEnsureMasterConcurrent(t *testing.T) {
	pool := storetest.New(t)
	ctx := context.Background()
	dir := t.TempDir()

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- EnsureMaster(ctx, pool, filepath.Join(dir, string(rune('a'+i))), discard)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureMaster: %v", err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users WHERE is_master").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d master accounts, want 1", n)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("%d password files written, want 1", len(files))
	}
}
