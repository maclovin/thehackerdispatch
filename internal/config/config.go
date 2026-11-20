package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

type Config struct {
	DatabaseURL        string
	BaseURL            *url.URL
	SecretsKey         []byte
	SlackBotToken      string
	OIDC               OIDC
	HTTPAddr           string
	MasterPasswordFile string
	LogLevel           slog.Level
}

type OIDC struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
}

func (o OIDC) Enabled() bool { return o.IssuerURL != "" }

const (
	DefaultHTTPAddr           = ":8080"
	DefaultMasterPasswordFile = "/tmp/thehackerdispatch/master-password"
)

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL:        getenv("DATABASE_URL"),
		SlackBotToken:      getenv("SLACK_BOT_TOKEN"),
		HTTPAddr:           withDefault(getenv("HTTP_ADDR"), DefaultHTTPAddr),
		MasterPasswordFile: withDefault(getenv("MASTER_PASSWORD_FILE"), DefaultMasterPasswordFile),
		OIDC: OIDC{
			IssuerURL:    getenv("OIDC_ISSUER_URL"),
			ClientID:     getenv("OIDC_CLIENT_ID"),
			ClientSecret: getenv("OIDC_CLIENT_SECRET"),
		},
	}
	var errs []error

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}

	if raw := getenv("BASE_URL"); raw == "" {
		errs = append(errs, errors.New("BASE_URL is required"))
	} else if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("BASE_URL must be an absolute http(s) URL, got %q", raw))
	} else {
		u.Path = strings.TrimSuffix(u.Path, "/")
		cfg.BaseURL = u
	}

	if raw := getenv("SECRETS_KEY"); raw == "" {
		errs = append(errs, errors.New("SECRETS_KEY is required (generate one with: openssl rand -base64 32)"))
	} else if key, err := base64.StdEncoding.DecodeString(raw); err != nil || len(key) != 32 {
		errs = append(errs, errors.New("SECRETS_KEY must be 32 bytes, base64-encoded (generate one with: openssl rand -base64 32)"))
	} else {
		cfg.SecretsKey = key
	}

	if o := cfg.OIDC; o.IssuerURL != "" || o.ClientID != "" || o.ClientSecret != "" {
		if o.IssuerURL == "" || o.ClientID == "" || o.ClientSecret == "" {
			errs = append(errs, errors.New("OIDC_ISSUER_URL, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET must be set together"))
		}
	}

	if raw := getenv("LOG_LEVEL"); raw != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(raw)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", raw))
		}
	}

	return cfg, errors.Join(errs...)
}

func withDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
