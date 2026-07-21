-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS sample_events (
    session_id UUID,
    sampled_at DateTime64(3, 'UTC'),
    sample_seq UInt32,
    probes_attempted UInt8,
    probes_ok UInt8,
    primary_ip IPv6,
    distinct_ips UInt8,
    ip_changed UInt8,
    new_ips UInt8,
    rtt_min_ms UInt32,
    rtt_med_ms UInt32,
    rtt_max_ms UInt32,
    egress_country LowCardinality(FixedString(2)),
    primary_category LowCardinality(String),
    primary_risk UInt8,
    probe_ips Array(IPv6),
    probe_rtts_ms Array(UInt32),
    probe_ok Array(UInt8),
    error String
)
ENGINE = MergeTree
PARTITION BY toDate(sampled_at)
ORDER BY (session_id, sampled_at)
TTL toDateTime(sampled_at) + INTERVAL 180 DAY;
-- +goose StatementEnd
-- +goose Down
DROP TABLE IF EXISTS sample_events;
