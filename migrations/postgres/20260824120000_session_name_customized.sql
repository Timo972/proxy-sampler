-- +goose Up
-- name_customized records whether a session's name was chosen by a person
-- rather than generated from its run name and variant params. The run rename
-- cascade consults it instead of comparing the stored name against the name it
-- would have generated: that comparison misclassifies a hand-picked name that
-- happens to equal what some later run name generates, and overwrites it.
ALTER TABLE sampling_sessions ADD COLUMN name_customized boolean NOT NULL DEFAULT false;

-- Standalone sessions are always named by the person who created them.
UPDATE sampling_sessions SET name_customized = true WHERE run_id IS NULL;

-- Existing run children have no recorded provenance, so infer it once here the
-- only way available: a child whose name still matches what its run's current
-- name generates is treated as generated, anything else as customized. This
-- mirrors the behaviour of VariantName (params sorted by key, "k=v" joined with
-- commas, empty params yielding the bare run name).
--
-- ORDER BY ... COLLATE "C" is required, not stylistic. VariantName sorts with
-- Go's sort.Strings, which compares bytewise, while a bare ORDER BY uses the
-- cluster's collation. Under a locale-aware one such as en_US.UTF-8, {"Z":"1",
-- "a":"2"} reconstructs as "a=2,Z=1" instead of "Z=1,a=2" and the child is
-- misclassified as customized, silently dropping it out of later run renames.
UPDATE sampling_sessions AS s
SET name_customized = true
FROM variation_runs AS r
WHERE s.run_id = r.id
  AND s.name IS DISTINCT FROM (
    CASE
      WHEN s.variant_params IS NULL OR s.variant_params = '{}'::jsonb THEN r.name
      ELSE r.name || ' (' || (
        SELECT string_agg(entry.key || '=' || entry.value, ',' ORDER BY entry.key COLLATE "C")
        FROM jsonb_each_text(s.variant_params) AS entry
      ) || ')'
    END
  );

-- +goose Down
ALTER TABLE sampling_sessions DROP COLUMN name_customized;
