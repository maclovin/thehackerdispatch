package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

const masterLockID = 0x6d6173746572

const MasterNickname = "admin"

func EnsureMaster(ctx context.Context, pool *pgxpool.Pool, passwordFile string, logger *slog.Logger) error {
	created := false
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", masterLockID); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM users WHERE is_master)").Scan(&exists); err != nil {
			return fmt.Errorf("check master: %w", err)
		}
		if exists {
			return nil
		}

		password, err := GeneratePassword()
		if err != nil {
			return err
		}
		hash, err := HashPassword(password)
		if err != nil {
			return err
		}
		var id int64
		err = tx.QueryRow(ctx, `
			INSERT INTO users (auth_mode, is_master, role, nickname, real_name, password_hash)
			VALUES ('local', true, 'admin', $1, 'Master admin', $2)
			RETURNING id`, MasterNickname, hash).Scan(&id)
		if err != nil {
			return fmt.Errorf("insert master: %w", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			ActorLabel: "system", Action: "master.created", Target: fmt.Sprintf("user:%d", id),
		}); err != nil {
			return err
		}
		// Before commit: if the file can't be written, no account is created and the next start retries.
		if err := writePasswordFile(passwordFile, password); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure master account: %w", err)
	}
	if created {
		logger.Warn("master account created; read its password from inside the container with: thehackerdispatch master-password",
			"login", MasterNickname)
	}
	return nil
}

func writePasswordFile(path, password string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create master password directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(password+"\n"), 0o600); err != nil {
		return fmt.Errorf("write master password file: %w", err)
	}
	return nil
}

var ErrNoMasterPasswordFile = errors.New("no master password file in this container: it was already used to log in, " +
	"or the master account was created by another pod (to start over, run DELETE FROM users WHERE is_master and restart)")

func ReadMasterPassword(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoMasterPasswordFile
	}
	if err != nil {
		return "", fmt.Errorf("read master password file: %w", err)
	}
	return string(b), nil
}
