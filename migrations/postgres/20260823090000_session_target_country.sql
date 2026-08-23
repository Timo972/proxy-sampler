-- +goose Up
ALTER TABLE sampling_sessions ADD COLUMN target_country text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sampling_sessions DROP COLUMN target_country;
