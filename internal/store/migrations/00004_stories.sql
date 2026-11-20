-- +goose Up
-- array_to_string is immutable for text[], which the generated search column needs.
-- +goose StatementBegin
CREATE FUNCTION text_array_to_string(text[]) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$ SELECT array_to_string($1, ' ') $$;
-- +goose StatementEnd

CREATE TABLE stories (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status         text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'publishing', 'published')),
    title          text NOT NULL,
    summary_md     text NOT NULL DEFAULT '',
    why_it_matters text NOT NULL DEFAULT '',
    tldr           text NOT NULL DEFAULT '',
    section_id     bigint NOT NULL REFERENCES sections (id),
    score          integer NOT NULL DEFAULT 0,
    priority       text NOT NULL DEFAULT 'normal' CHECK (priority IN ('normal', 'critical')),
    entities       text[] NOT NULL DEFAULT '{}',
    search         tsvector GENERATED ALWAYS AS (
                       to_tsvector('english', title || ' ' || tldr || ' ' || text_array_to_string(entities))
                   ) STORED,
    model          text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,
    expires_at     timestamptz
);

CREATE INDEX stories_published ON stories (published_at DESC) WHERE status = 'published';
CREATE INDEX stories_expires_at ON stories (expires_at);
CREATE INDEX stories_search ON stories USING gin (search);

CREATE TABLE story_links (
    story_id   bigint NOT NULL REFERENCES stories (id) ON DELETE CASCADE,
    url        text NOT NULL,
    title      text NOT NULL,
    source_id  bigint REFERENCES sources (id) ON DELETE SET NULL,
    is_primary boolean NOT NULL DEFAULT false,
    added_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (story_id, url)
);

CREATE TABLE story_updates (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    story_id   bigint NOT NULL REFERENCES stories (id) ON DELETE CASCADE,
    note       text NOT NULL,
    url        text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE raw_items ADD COLUMN story_id bigint REFERENCES stories (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE raw_items DROP COLUMN story_id;
DROP TABLE story_updates;
DROP TABLE story_links;
DROP TABLE stories;
DROP FUNCTION text_array_to_string(text[]);
