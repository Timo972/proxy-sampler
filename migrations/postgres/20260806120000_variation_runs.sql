-- +goose Up
CREATE TABLE variation_runs (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    template_ciphertext bytea NOT NULL,
    template_nonce bytea NOT NULL,
    template_display text NOT NULL,
    axes jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE sampling_sessions ADD COLUMN run_id uuid REFERENCES variation_runs(id) ON DELETE CASCADE;
ALTER TABLE sampling_sessions ADD COLUMN variant_params jsonb;
ALTER TABLE sampling_sessions ADD COLUMN cell_key text;
CREATE INDEX sampling_sessions_run_idx ON sampling_sessions (run_id);

-- +goose Down
DROP INDEX IF EXISTS sampling_sessions_run_idx;
ALTER TABLE sampling_sessions DROP COLUMN IF EXISTS cell_key;
ALTER TABLE sampling_sessions DROP COLUMN IF EXISTS variant_params;
ALTER TABLE sampling_sessions DROP COLUMN IF EXISTS run_id;
DROP TABLE IF EXISTS variation_runs;
