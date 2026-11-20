-- +goose Up
CREATE UNIQUE INDEX sections_name ON sections (lower(name));

CREATE TABLE prompts (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    section_id bigint REFERENCES sections (id) ON DELETE CASCADE,
    version    integer NOT NULL,
    body       text NOT NULL,
    author     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (section_id, version)
);

-- +goose Down
DROP TABLE prompts;
DROP INDEX sections_name;
