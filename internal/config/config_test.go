package config

import (
	"log/slog"
	"strings"
	"testing"
)

const validKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func minimal() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://u:p@db/hd",
		"BASE_URL":     "https://news.example.com/",
		"SECRETS_KEY":  validKey,
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(minimal()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != DefaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, DefaultHTTPAddr)
	}
	if cfg.MasterPasswordFile != DefaultMasterPasswordFile {
		t.Errorf("MasterPasswordFile = %q, want %q", cfg.MasterPasswordFile, DefaultMasterPasswordFile)
	}
	if got := cfg.BaseURL.String(); got != "https://news.example.com" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", got)
	}
	if len(cfg.SecretsKey) != 32 {
		t.Errorf("SecretsKey has %d bytes, want 32", len(cfg.SecretsKey))
	}
	if cfg.OIDC.Enabled() {
		t.Error("OIDC enabled without OIDC variables")
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		change  map[string]string
		wantErr string
	}{
		{"missing database", map[string]string{"DATABASE_URL": ""}, "DATABASE_URL is required"},
		{"missing base url", map[string]string{"BASE_URL": ""}, "BASE_URL is required"},
		{"relative base url", map[string]string{"BASE_URL": "/news"}, "BASE_URL must be an absolute"},
		{"missing secrets key", map[string]string{"SECRETS_KEY": ""}, "SECRETS_KEY is required"},
		{"short secrets key", map[string]string{"SECRETS_KEY": "c2hvcnQ="}, "SECRETS_KEY must be 32 bytes"},
		{"partial oidc", map[string]string{"OIDC_ISSUER_URL": "https://sso/realms/x"}, "must be set together"},
		{"bad log level", map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := minimal()
			for k, v := range tt.change {
				m[k] = v
			}
			_, err := Load(env(m))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadOIDC(t *testing.T) {
	m := minimal()
	m["OIDC_ISSUER_URL"] = "https://sso.example.com/realms/company"
	m["OIDC_CLIENT_ID"] = "thehackerdispatch"
	m["OIDC_CLIENT_SECRET"] = "s3cret"
	cfg, err := Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OIDC.Enabled() {
		t.Error("OIDC not enabled with all variables set")
	}
}
