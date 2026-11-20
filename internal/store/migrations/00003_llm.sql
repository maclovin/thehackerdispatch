-- +goose Up
CREATE TABLE llm_providers (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name          text NOT NULL UNIQUE,
    type          text NOT NULL CHECK (type IN ('anthropic', 'openai', 'openai-compatible')),
    base_url      text NOT NULL DEFAULT '',
    api_key_enc   bytea NOT NULL,
    api_key_last4 text NOT NULL,
    enabled       boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE llm_models (
    id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id          bigint NOT NULL REFERENCES llm_providers (id) ON DELETE CASCADE,
    model_id             text NOT NULL,
    input_price          numeric(10, 4) NOT NULL DEFAULT 0,
    output_price         numeric(10, 4) NOT NULL DEFAULT 0,
    cached_price         numeric(10, 4) NOT NULL DEFAULT 0,
    supports_json_schema boolean NOT NULL DEFAULT true,
    enabled              boolean NOT NULL DEFAULT true,
    UNIQUE (provider_id, model_id)
);

CREATE TABLE llm_calls (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    stage             text NOT NULL,
    provider_id       bigint REFERENCES llm_providers (id) ON DELETE SET NULL,
    model_id          text NOT NULL,
    input_tokens      bigint NOT NULL DEFAULT 0,
    output_tokens     bigint NOT NULL DEFAULT 0,
    cache_read_tokens bigint NOT NULL DEFAULT 0,
    cost_usd          numeric(12, 6) NOT NULL DEFAULT 0,
    latency_ms        integer NOT NULL DEFAULT 0,
    outcome           text NOT NULL,
    error             text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX llm_calls_created_at ON llm_calls (created_at);

-- +goose Down
DROP TABLE llm_calls;
DROP TABLE llm_models;
DROP TABLE llm_providers;
