-- +goose Up
ALTER TABLE sampling_sessions ADD COLUMN sequence_offset integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sampling_sessions DROP COLUMN sequence_offset;
