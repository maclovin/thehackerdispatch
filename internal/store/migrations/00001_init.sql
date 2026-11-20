-- +goose Up
CREATE TABLE users (
    id                       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    auth_mode                text NOT NULL CHECK (auth_mode IN ('sso', 'local')),
    email                    text UNIQUE CHECK (email = lower(email)),
    sso_subject              text UNIQUE,
    password_hash            text,
    password_temp_expires_at timestamptz,
    must_rotate              boolean NOT NULL DEFAULT false,
    failed_logins            integer NOT NULL DEFAULT 0,
    locked_until             timestamptz,
    is_master                boolean NOT NULL DEFAULT false,
    real_name                text NOT NULL DEFAULT '',
    nickname                 text UNIQUE,
    role                     text NOT NULL DEFAULT 'member' CHECK (role IN ('member', 'admin')),
    preferred_sections       text[] NOT NULL DEFAULT '{}',
    theme                    text NOT NULL DEFAULT 'system' CHECK (theme IN ('system', 'light', 'dark')),
    banned_at                timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    last_login_at            timestamptz,
    CHECK (is_master OR email IS NOT NULL)
);

CREATE UNIQUE INDEX users_single_master ON users (is_master) WHERE is_master;

CREATE TABLE sessions (
    id_hash      bytea PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    ip           text NOT NULL DEFAULT '',
    user_agent   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);

CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_id    bigint REFERENCES users (id) ON DELETE SET NULL,
    actor_label text NOT NULL,
    action      text NOT NULL,
    target      text NOT NULL DEFAULT '',
    details     jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_created_at ON audit_log (created_at);

-- +goose Down
DROP TABLE audit_log;
DROP TABLE settings;
DROP TABLE sessions;
DROP TABLE users;
