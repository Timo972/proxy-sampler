-- +goose Up
CREATE TABLE sampling_sessions (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    proxy_ciphertext bytea NOT NULL,
    proxy_nonce bytea NOT NULL,
    proxy_display text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('sticky', 'pool')),
    cadence_seconds integer NOT NULL CHECK (cadence_seconds > 0),
    probes_per_sample integer NOT NULL CHECK (probes_per_sample BETWEEN 1 AND 255),
    probe_target text NOT NULL,
    dial_timeout_ms integer NOT NULL CHECK (dial_timeout_ms > 0),
    max_samples integer CHECK (max_samples > 0),
    max_duration_seconds integer CHECK (max_duration_seconds > 0),
    status text NOT NULL CHECK (status IN ('running', 'stopped', 'finished')),
    samples_taken integer NOT NULL DEFAULT 0,
    probes_ok bigint NOT NULL DEFAULT 0,
    probes_total bigint NOT NULL DEFAULT 0,
    distinct_ips integer NOT NULL DEFAULT 0,
    last_sample_at timestamptz,
    last_primary_ip inet,
    last_rtt_ms integer,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    stopped_at timestamptz,
    claimed_by text,
    lease_until timestamptz
);
CREATE TABLE session_ips (
    session_id uuid NOT NULL REFERENCES sampling_sessions(id) ON DELETE CASCADE,
    ip inet NOT NULL, first_seen timestamptz NOT NULL DEFAULT now(),
    last_seen timestamptz NOT NULL DEFAULT now(), hit_count bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, ip)
);
CREATE TABLE ip_reputation_cache (
    ip inet PRIMARY KEY, country text, region text, city text, isp text, asn text,
    is_mobile boolean, ipapi_proxy boolean, ipapi_hosting boolean,
    pc_type text, pc_proxy boolean, risk_score integer CHECK (risk_score BETWEEN 0 AND 100),
    greynoise_class text, sfs_appears boolean, sfs_frequency integer,
    dnsbl_listed boolean, dnsbl_hits text, category text,
    raw jsonb NOT NULL DEFAULT '{}'::jsonb,
    first_seen timestamptz NOT NULL DEFAULT now(), refreshed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sampling_sessions_status_created_idx ON sampling_sessions (status, created_at DESC);
CREATE INDEX session_ips_session_last_seen_idx ON session_ips (session_id, last_seen DESC);

-- +goose Down
DROP TABLE IF EXISTS session_ips;
DROP TABLE IF EXISTS ip_reputation_cache;
DROP TABLE IF EXISTS sampling_sessions;
