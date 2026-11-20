-- +goose Up
CREATE TABLE sections (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9-]+$'),
    name text NOT NULL,
    sort integer NOT NULL DEFAULT 0
);

INSERT INTO sections (slug, name, sort) VALUES
    ('security', 'Security', 1),
    ('tech', 'Tech', 2),
    ('business', 'Business', 3),
    ('engineering', 'Engineering', 4);

CREATE TABLE sources (
    id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                 text NOT NULL,
    url                  text NOT NULL UNIQUE,
    section_id           bigint NOT NULL REFERENCES sections (id),
    enabled              boolean NOT NULL DEFAULT true,
    user_agent           text NOT NULL DEFAULT '',
    etag                 text NOT NULL DEFAULT '',
    last_modified        text NOT NULL DEFAULT '',
    cursor_at            timestamptz,
    last_guids           text[] NOT NULL DEFAULT '{}',
    last_fetch_at        timestamptz,
    last_status          text NOT NULL DEFAULT '',
    consecutive_failures integer NOT NULL DEFAULT 0,
    created_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE raw_items (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id    bigint NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    guid_hash    text NOT NULL,
    url          text NOT NULL,
    title        text NOT NULL,
    snippet      text NOT NULL DEFAULT '',
    published_at timestamptz,
    fetched_at   timestamptz NOT NULL DEFAULT now(),
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'curated', 'failed')),
    decision     text,
    reason       text NOT NULL DEFAULT '',
    attempts     integer NOT NULL DEFAULT 0,
    UNIQUE (source_id, guid_hash)
);

CREATE INDEX raw_items_pending ON raw_items (fetched_at) WHERE status = 'pending';
CREATE INDEX raw_items_fetched_at ON raw_items (fetched_at);

CREATE TABLE job_runs (
    name             text PRIMARY KEY,
    last_started_at  timestamptz NOT NULL,
    last_finished_at timestamptz,
    last_error       text NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE job_runs;
DROP TABLE raw_items;
DROP TABLE sources;
DROP TABLE sections;
