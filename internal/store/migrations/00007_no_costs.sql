-- +goose Up
ALTER TABLE llm_models DROP COLUMN input_price, DROP COLUMN output_price, DROP COLUMN cached_price;
ALTER TABLE llm_calls DROP COLUMN cost_usd;

-- +goose Down
ALTER TABLE llm_calls ADD COLUMN cost_usd numeric(12, 6) NOT NULL DEFAULT 0;
ALTER TABLE llm_models
    ADD COLUMN input_price numeric(10, 4) NOT NULL DEFAULT 0,
    ADD COLUMN output_price numeric(10, 4) NOT NULL DEFAULT 0,
    ADD COLUMN cached_price numeric(10, 4) NOT NULL DEFAULT 0;
