-- name: ReputationByIP :one
SELECT ip::text AS ip, country, region, city, isp, asn, is_mobile, ipapi_proxy,
  ipapi_hosting, pc_type, pc_proxy, risk_score, greynoise_class, sfs_appears,
  sfs_frequency, dnsbl_listed, dnsbl_hits, category, raw, first_seen, refreshed_at
FROM ip_reputation_cache WHERE ip = sqlc.arg(ip)::text::inet;

-- name: UpsertReputation :exec
INSERT INTO ip_reputation_cache (
  ip, country, region, city, isp, asn, is_mobile, ipapi_proxy, ipapi_hosting,
  pc_type, pc_proxy, risk_score, greynoise_class, sfs_appears, sfs_frequency,
  dnsbl_listed, dnsbl_hits, category, raw, first_seen, refreshed_at
) VALUES (
  sqlc.arg(ip)::text::inet, NULLIF(sqlc.arg(country)::text, ''),
  NULLIF(sqlc.arg(region)::text, ''), NULLIF(sqlc.arg(city)::text, ''),
  NULLIF(sqlc.arg(isp)::text, ''), NULLIF(sqlc.arg(asn)::text, ''),
  sqlc.narg(is_mobile), sqlc.narg(ipapi_proxy), sqlc.narg(ipapi_hosting),
  NULLIF(sqlc.arg(pc_type)::text, ''), sqlc.narg(pc_proxy), sqlc.narg(risk_score),
  NULLIF(sqlc.arg(greynoise_class)::text, ''), sqlc.narg(sfs_appears),
  sqlc.narg(sfs_frequency), sqlc.narg(dnsbl_listed),
  NULLIF(sqlc.arg(dnsbl_hits)::text, ''), NULLIF(sqlc.arg(category)::text, ''),
  sqlc.arg(raw), sqlc.arg(first_seen), sqlc.arg(refreshed_at)
)
ON CONFLICT (ip) DO UPDATE SET
  country = EXCLUDED.country, region = EXCLUDED.region, city = EXCLUDED.city,
  isp = EXCLUDED.isp, asn = EXCLUDED.asn, is_mobile = EXCLUDED.is_mobile,
  ipapi_proxy = EXCLUDED.ipapi_proxy, ipapi_hosting = EXCLUDED.ipapi_hosting,
  pc_type = EXCLUDED.pc_type, pc_proxy = EXCLUDED.pc_proxy,
  risk_score = EXCLUDED.risk_score, greynoise_class = EXCLUDED.greynoise_class,
  sfs_appears = EXCLUDED.sfs_appears, sfs_frequency = EXCLUDED.sfs_frequency,
  dnsbl_listed = EXCLUDED.dnsbl_listed, dnsbl_hits = EXCLUDED.dnsbl_hits,
  category = EXCLUDED.category, raw = EXCLUDED.raw, refreshed_at = EXCLUDED.refreshed_at;
