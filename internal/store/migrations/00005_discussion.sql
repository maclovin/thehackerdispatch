-- +goose Up
CREATE TABLE comments (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    story_id   bigint NOT NULL REFERENCES stories (id) ON DELETE CASCADE,
    parent_id  bigint REFERENCES comments (id) ON DELETE CASCADE,
    user_id    bigint REFERENCES users (id) ON DELETE SET NULL,
    body_md    text NOT NULL,
    deleted    boolean NOT NULL DEFAULT false,
    points     integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    edited_at  timestamptz
);

CREATE INDEX comments_story ON comments (story_id, created_at);
CREATE INDEX comments_user_recent ON comments (user_id, created_at);

CREATE TABLE comment_votes (
    user_id    bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    comment_id bigint NOT NULL REFERENCES comments (id) ON DELETE CASCADE,
    value      smallint NOT NULL CHECK (value IN (-1, 1)),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, comment_id)
);

CREATE INDEX comment_votes_recent ON comment_votes (user_id, created_at);

ALTER TABLE stories ADD COLUMN comment_count integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE stories DROP COLUMN comment_count;
DROP TABLE comment_votes;
DROP TABLE comments;
