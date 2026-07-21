-- name: InsertSession :exec
INSERT INTO sampling_sessions (
  id, name, proxy_ciphertext, proxy_nonce, proxy_display, mode,
  cadence_seconds, probes_per_sample, probe_target, dial_timeout_ms,
  max_samples, max_duration_seconds, status, samples_taken, probes_ok,
  probes_total, distinct_ips, last_sample_at, last_primary_ip, last_rtt_ms,
  last_error, created_at, started_at, stopped_at
) VALUES (
  sqlc.arg(id), sqlc.arg(name), sqlc.arg(proxy_ciphertext), sqlc.arg(proxy_nonce),
  sqlc.arg(proxy_display), sqlc.arg(mode), sqlc.arg(cadence_seconds),
  sqlc.arg(probes_per_sample), sqlc.arg(probe_target), sqlc.arg(dial_timeout_ms),
  sqlc.narg(max_samples), sqlc.narg(max_duration_seconds), sqlc.arg(status),
  sqlc.arg(samples_taken), sqlc.arg(probes_ok), sqlc.arg(probes_total),
  sqlc.arg(distinct_ips), sqlc.narg(last_sample_at),
  NULLIF(sqlc.arg(last_primary_ip)::text, '')::inet, sqlc.narg(last_rtt_ms),
  NULLIF(sqlc.arg(last_error)::text, ''), sqlc.arg(created_at),
  sqlc.narg(started_at), sqlc.narg(stopped_at)
);

-- name: Sessions :many
SELECT
  s.id, s.name, s.proxy_ciphertext, s.proxy_nonce, s.proxy_display, s.mode,
  s.cadence_seconds, s.probes_per_sample, s.probe_target, s.dial_timeout_ms,
  s.max_samples, s.max_duration_seconds, s.status, s.samples_taken, s.probes_ok,
  s.probes_total, s.distinct_ips, s.last_sample_at, CAST(COALESCE(s.last_primary_ip::text, '') AS text) AS last_primary_ip,
  COALESCE(r.category, '') AS last_category, s.last_rtt_ms, s.last_error,
  s.created_at, s.started_at, s.stopped_at
FROM sampling_sessions AS s
LEFT JOIN ip_reputation_cache AS r ON r.ip = s.last_primary_ip
ORDER BY s.created_at DESC;

-- name: SessionByID :one
SELECT
  s.id, s.name, s.proxy_ciphertext, s.proxy_nonce, s.proxy_display, s.mode,
  s.cadence_seconds, s.probes_per_sample, s.probe_target, s.dial_timeout_ms,
  s.max_samples, s.max_duration_seconds, s.status, s.samples_taken, s.probes_ok,
  s.probes_total, s.distinct_ips, s.last_sample_at, CAST(COALESCE(s.last_primary_ip::text, '') AS text) AS last_primary_ip,
  COALESCE(r.category, '') AS last_category, s.last_rtt_ms, s.last_error,
  s.created_at, s.started_at, s.stopped_at
FROM sampling_sessions AS s
LEFT JOIN ip_reputation_cache AS r ON r.ip = s.last_primary_ip
WHERE s.id = sqlc.arg(id);

-- name: RunningSessions :many
SELECT
  s.id, s.name, s.proxy_ciphertext, s.proxy_nonce, s.proxy_display, s.mode,
  s.cadence_seconds, s.probes_per_sample, s.probe_target, s.dial_timeout_ms,
  s.max_samples, s.max_duration_seconds, s.status, s.samples_taken, s.probes_ok,
  s.probes_total, s.distinct_ips, s.last_sample_at, CAST(COALESCE(s.last_primary_ip::text, '') AS text) AS last_primary_ip,
  COALESCE(r.category, '') AS last_category, s.last_rtt_ms, s.last_error,
  s.created_at, s.started_at, s.stopped_at
FROM sampling_sessions AS s
LEFT JOIN ip_reputation_cache AS r ON r.ip = s.last_primary_ip
WHERE s.status = 'running'
ORDER BY s.created_at ASC;

-- name: StopSession :execrows
UPDATE sampling_sessions
SET status = 'stopped', stopped_at = sqlc.arg(stopped_at)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: FinishSession :execrows
UPDATE sampling_sessions
SET status = 'finished', stopped_at = sqlc.arg(stopped_at)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: DeleteSession :exec
DELETE FROM sampling_sessions WHERE id = sqlc.arg(id);

-- name: UpdateSessionSnapshot :exec
UPDATE sampling_sessions SET
  samples_taken = sqlc.arg(samples_taken), probes_ok = sqlc.arg(probes_ok),
  probes_total = sqlc.arg(probes_total), distinct_ips = sqlc.arg(distinct_ips),
  last_sample_at = sqlc.narg(last_sample_at),
  last_primary_ip = NULLIF(sqlc.arg(last_primary_ip)::text, '')::inet,
  last_rtt_ms = sqlc.narg(last_rtt_ms), last_error = NULLIF(sqlc.arg(last_error)::text, '')
WHERE id = sqlc.arg(id);

-- name: UpsertSessionIP :exec
INSERT INTO session_ips (session_id, ip, first_seen, last_seen, hit_count)
VALUES (sqlc.arg(session_id), sqlc.arg(ip)::text::inet, sqlc.arg(seen_at), sqlc.arg(seen_at), sqlc.arg(hits))
ON CONFLICT (session_id, ip) DO UPDATE
SET last_seen = EXCLUDED.last_seen, hit_count = session_ips.hit_count + EXCLUDED.hit_count;

-- name: SessionIPs :many
SELECT
  si.ip::text AS ip, si.first_seen, si.last_seen, si.hit_count,
  CAST(COALESCE(r.ip::text, '') AS text) AS reputation_ip, r.country, r.region, r.city, r.isp, r.asn,
  r.is_mobile, r.ipapi_proxy, r.ipapi_hosting, r.pc_type, r.pc_proxy,
  r.risk_score, r.greynoise_class, r.sfs_appears, r.sfs_frequency,
  r.dnsbl_listed, r.dnsbl_hits, r.category, r.raw, r.first_seen AS reputation_first_seen,
  r.refreshed_at
FROM session_ips AS si
LEFT JOIN ip_reputation_cache AS r ON r.ip = si.ip
WHERE si.session_id = sqlc.arg(session_id)
ORDER BY si.last_seen DESC;
