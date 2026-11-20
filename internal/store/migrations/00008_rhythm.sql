-- +goose Up
ALTER TABLE sources ADD COLUMN retry_after timestamptz;

CREATE TABLE pacing_arrivals (
    arrived_at timestamptz NOT NULL DEFAULT now(),
    score      smallint NOT NULL
);

CREATE INDEX pacing_arrivals_arrived_at ON pacing_arrivals (arrived_at);

CREATE TABLE pacing_state (
    id           boolean PRIMARY KEY DEFAULT true CHECK (id),
    next_slot_at timestamptz NOT NULL
);

DELETE FROM job_runs WHERE name = 'publish';

CREATE TABLE notification_channels (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name          text NOT NULL,
    slack_channel text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX notification_channels_name ON notification_channels (lower(name));

ALTER TABLE sections ADD COLUMN notification_channel_id bigint REFERENCES notification_channels (id) ON DELETE SET NULL;

ALTER TABLE stories ADD COLUMN slack_channel text NOT NULL DEFAULT '', ADD COLUMN slack_ts text NOT NULL DEFAULT '';

CREATE TABLE slack_outbox (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    story_id        bigint NOT NULL REFERENCES stories (id) ON DELETE CASCADE,
    update_id       bigint REFERENCES story_updates (id) ON DELETE CASCADE,
    channel_id      bigint REFERENCES notification_channels (id) ON DELETE SET NULL,
    slack_channel   text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz
);

CREATE INDEX slack_outbox_pending ON slack_outbox (next_attempt_at) WHERE status = 'pending';
CREATE INDEX slack_outbox_channel ON slack_outbox (channel_id, id);

-- +goose Down
DROP TABLE slack_outbox;
ALTER TABLE stories DROP COLUMN slack_channel, DROP COLUMN slack_ts;
ALTER TABLE sections DROP COLUMN notification_channel_id;
DROP TABLE notification_channels;
DROP TABLE pacing_state;
DROP TABLE pacing_arrivals;
ALTER TABLE sources DROP COLUMN retry_after;
