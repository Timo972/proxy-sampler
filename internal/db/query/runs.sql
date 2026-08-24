-- name: InsertRun :exec
INSERT INTO variation_runs (id, name, template_ciphertext, template_nonce, template_display, axes, created_at)
VALUES (sqlc.arg(id), sqlc.arg(name), sqlc.arg(template_ciphertext), sqlc.arg(template_nonce),
  sqlc.arg(template_display), sqlc.arg(axes), sqlc.arg(created_at));

-- name: InsertRunSession :exec
INSERT INTO sampling_sessions (
  id, name, proxy_ciphertext, proxy_nonce, proxy_display, mode,
  cadence_seconds, probes_per_sample, probe_target, dial_timeout_ms,
  max_samples, max_duration_seconds, status, samples_taken, probes_ok,
  probes_total, distinct_ips, last_sample_at, last_primary_ip, last_rtt_ms,
  last_error, created_at, started_at, stopped_at, sequence_offset,
  target_country, run_id, variant_params, cell_key
) VALUES (
  sqlc.arg(id), sqlc.arg(name), sqlc.arg(proxy_ciphertext), sqlc.arg(proxy_nonce),
  sqlc.arg(proxy_display), sqlc.arg(mode), sqlc.arg(cadence_seconds),
  sqlc.arg(probes_per_sample), sqlc.arg(probe_target), sqlc.arg(dial_timeout_ms),
  sqlc.narg(max_samples), sqlc.narg(max_duration_seconds), sqlc.arg(status),
  sqlc.arg(samples_taken), sqlc.arg(probes_ok), sqlc.arg(probes_total),
  sqlc.arg(distinct_ips), sqlc.narg(last_sample_at),
  NULLIF(sqlc.arg(last_primary_ip)::text, '')::inet, sqlc.narg(last_rtt_ms),
  NULLIF(sqlc.arg(last_error)::text, ''), sqlc.arg(created_at),
  sqlc.narg(started_at), sqlc.narg(stopped_at), sqlc.arg(sequence_offset),
  sqlc.arg(target_country), sqlc.arg(run_id), sqlc.arg(variant_params), sqlc.arg(cell_key)
);

-- name: Runs :many
SELECT
  run.id, run.name, run.template_ciphertext, run.template_nonce, run.template_display,
  run.axes, run.created_at,
  COUNT(s.id) AS variant_count,
  (SELECT COUNT(DISTINCT si.ip) FROM session_ips AS si
     JOIN sampling_sessions AS c ON c.id = si.session_id
     WHERE c.run_id = run.id)::bigint AS distinct_ips,
  COUNT(*) FILTER (WHERE s.status = 'running') AS running_count,
  COUNT(*) FILTER (WHERE s.status = 'finished') AS finished_count
FROM variation_runs AS run
LEFT JOIN sampling_sessions AS s ON s.run_id = run.id
GROUP BY run.id
ORDER BY run.created_at DESC;

-- name: RunByID :one
SELECT
  run.id, run.name, run.template_ciphertext, run.template_nonce, run.template_display,
  run.axes, run.created_at,
  COUNT(s.id) AS variant_count,
  (SELECT COUNT(DISTINCT si.ip) FROM session_ips AS si
     JOIN sampling_sessions AS c ON c.id = si.session_id
     WHERE c.run_id = run.id)::bigint AS distinct_ips,
  COUNT(*) FILTER (WHERE s.status = 'running') AS running_count,
  COUNT(*) FILTER (WHERE s.status = 'finished') AS finished_count
FROM variation_runs AS run
LEFT JOIN sampling_sessions AS s ON s.run_id = run.id
WHERE run.id = sqlc.arg(id)
GROUP BY run.id;

-- name: RunSessions :many
SELECT
  s.id, s.name, COALESCE(s.variant_params, '{}'::jsonb) AS variant_params,
  COALESCE(s.cell_key, '') AS cell_key, s.target_country, s.status,
  s.samples_taken, s.probes_ok, s.probes_total, s.distinct_ips
FROM sampling_sessions AS s
WHERE s.run_id = sqlc.arg(run_id)
ORDER BY s.created_at ASC, s.id ASC;

-- name: SessionsByRun :many
SELECT s.id
FROM sampling_sessions AS s
WHERE s.run_id = sqlc.arg(run_id)
ORDER BY s.created_at ASC, s.id ASC;

-- name: RunIPObservations :many
SELECT
  si.session_id, si.ip::text AS ip, si.hit_count, si.first_seen, si.last_seen,
  CAST(COALESCE(r.ip::text, '') AS text) AS reputation_ip, r.country, r.region, r.city, r.isp, r.asn,
  r.is_mobile, r.ipapi_proxy, r.ipapi_hosting, r.pc_type, r.pc_proxy,
  r.risk_score, r.greynoise_class, r.sfs_appears, r.sfs_frequency,
  r.dnsbl_listed, r.dnsbl_hits, r.category, r.raw, r.first_seen AS reputation_first_seen,
  r.refreshed_at
FROM session_ips AS si
JOIN sampling_sessions AS s ON s.id = si.session_id
LEFT JOIN ip_reputation_cache AS r ON r.ip = si.ip
WHERE s.run_id = sqlc.arg(run_id)
ORDER BY si.last_seen DESC
LIMIT sqlc.arg(row_limit);

-- name: LockRunForRename :one
-- FOR NO KEY UPDATE, not FOR UPDATE: renaming never changes the run's key, and
-- the weaker mode still serializes concurrent run renames against each other
-- while leaving the KEY SHARE lock a child row update takes for its run_id
-- foreign key free. FOR UPDATE would block that, deadlocking this transaction
-- against a concurrent session rename that already holds the child's row lock.
SELECT run.name
FROM variation_runs AS run
WHERE run.id = sqlc.arg(id)
FOR NO KEY UPDATE;

-- name: RenameRun :exec
UPDATE variation_runs
SET name = sqlc.arg(name)
WHERE id = sqlc.arg(id);

-- name: RenameGeneratedRunSession :execrows
-- Conditional on the name the cascade read, so a session rename that commits
-- between that read and this write is preserved rather than overwritten.
UPDATE sampling_sessions
SET name = sqlc.arg(name)
WHERE id = sqlc.arg(id) AND name = sqlc.arg(expected_name);

-- name: RunSessionNames :many
SELECT s.id, s.name, COALESCE(s.variant_params, '{}'::jsonb) AS variant_params
FROM sampling_sessions AS s
WHERE s.run_id = sqlc.arg(run_id)
ORDER BY s.created_at ASC, s.id ASC;

-- name: DeleteRun :exec
DELETE FROM variation_runs WHERE id = sqlc.arg(id);
