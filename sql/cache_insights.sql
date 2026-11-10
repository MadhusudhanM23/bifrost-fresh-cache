-- cache_insights: ask the semantic cache questions in plain SQL.
--
-- Safe to run again and again (CREATE ... IF NOT EXISTS / CREATE OR REPLACE).
-- It only READS the cache table and creates its own schema. To remove everything:
--   DROP SCHEMA cache_insights CASCADE;      (see sql/drop.sql)
--
-- Privacy: no view here shows a prompt or an answer. Answers are exposed by
-- LENGTH only. (The cache never stores the question text at all.)
--
-- The cache table is public."BifrostSemanticCachePlugin" (its table comment is
-- 'bifrost_vectorstore'): id text, embedding vector(256), metadata jsonb with the
-- keys cache_key, provider, model, params_hash, expires_at (unix seconds),
-- stream_chunks, response, from_bifrost_semantic_cache_plugin.
-- The table has NO created-at column. "When was it stored" is therefore an
-- estimate:  expires_at minus the cache lifetime (settings.cache_ttl_seconds,
-- 1800 = the "30m" ttl in docker/bifrost-config.json). If the optional file
-- cache-ages.json was loaded (sql/load_logs.*), the exact first-seen time wins.

SET client_min_messages = warning;   -- hide the 'already exists, skipping' notices

CREATE SCHEMA IF NOT EXISTS cache_insights;

-- ---------------------------------------------------------------- settings
CREATE TABLE IF NOT EXISTS cache_insights.settings (
    name  text PRIMARY KEY,
    value text NOT NULL
);
INSERT INTO cache_insights.settings(name, value)
VALUES ('cache_ttl_seconds', '1800')
ON CONFLICT (name) DO NOTHING;
COMMENT ON TABLE cache_insights.settings IS
  'cache_ttl_seconds: the cache lifetime used to estimate when an entry was stored. Change it with UPDATE if your ttl differs.';

-- ---------------------------------------------------------- staging tables
-- Filled by sql/load_logs.ps1 / .sh (a reload replaces the content).
CREATE TABLE IF NOT EXISTS cache_insights.decisions_raw (doc jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS cache_insights.audits_raw    (doc jsonb NOT NULL);
-- cache-ages.json is ONE json object: {"version":1,"entries":{"<cache id>":"<time>",...}}
CREATE TABLE IF NOT EXISTS cache_insights.cache_ages_raw (doc jsonb NOT NULL);

-- ------------------------------------------------------------ entries (base)
-- One readable row per cache entry. Other views build on this one.
CREATE OR REPLACE VIEW cache_insights.entries AS
SELECT
    c.id                                              AS cache_id,
    c.metadata->>'cache_key'                          AS cache_key,
    c.metadata->>'provider'                           AS provider,
    c.metadata->>'model'                              AS model,
    (c.embedding IS NOT NULL)                         AS has_embedding,
    to_timestamp((c.metadata->>'expires_at')::double precision) AS expires_at,
    (to_timestamp((c.metadata->>'expires_at')::double precision) < now()) AS is_expired,
    COALESCE(
        (SELECT (a.value)::timestamptz
           FROM cache_insights.cache_ages_raw r,
                LATERAL jsonb_each_text(r.doc->'entries') a
          WHERE a.key = c.id
          LIMIT 1),
        to_timestamp((c.metadata->>'expires_at')::double precision)
          - make_interval(secs => (SELECT value::int FROM cache_insights.settings WHERE name = 'cache_ttl_seconds'))
    )                                                 AS stored_at_estimate,
    length(c.metadata->>'response')                   AS answer_chars,
    octet_length(c.metadata::text)                    AS metadata_bytes,
    pg_column_size(c.metadata)                        AS metadata_bytes_on_disk,
    COALESCE(pg_column_size(c.embedding), 0)          AS embedding_bytes,
    round(extract(epoch FROM to_timestamp((c.metadata->>'expires_at')::double precision) - now()) / 60)::int AS minutes_until_expiry
FROM public."BifrostSemanticCachePlugin" c;
COMMENT ON VIEW cache_insights.entries IS
  'One row per cache entry. Answer text is never shown, only answer_chars. stored_at_estimate = expires_at - ttl unless cache-ages.json was loaded.';

-- ---------------------------------------------------------------- overview
CREATE OR REPLACE VIEW cache_insights.overview AS
SELECT
    count(*)                                                AS entries,
    count(*) FILTER (WHERE NOT has_embedding)               AS direct_only_entries,
    count(*) FILTER (WHERE has_embedding)                   AS entries_with_embedding,
    count(*) FILTER (WHERE is_expired)                      AS expired_but_still_stored,
    count(*) FILTER (WHERE NOT is_expired)                  AS still_valid,
    min(stored_at_estimate)                                 AS oldest_stored_estimate,
    max(stored_at_estimate)                                 AS newest_stored_estimate,
    (SELECT pg_relation_size(c.oid) FROM pg_class c
       WHERE c.oid = 'public."BifrostSemanticCachePlugin"'::regclass)  AS table_bytes,
    (SELECT pg_relation_size(c.reltoastrelid) FROM pg_class c
       WHERE c.oid = 'public."BifrostSemanticCachePlugin"'::regclass)  AS toast_bytes,
    pg_indexes_size('public."BifrostSemanticCachePlugin"'::regclass)   AS index_bytes,
    pg_total_relation_size('public."BifrostSemanticCachePlugin"'::regclass) AS total_bytes,
    pg_size_pretty(pg_total_relation_size('public."BifrostSemanticCachePlugin"'::regclass)) AS total_size
FROM cache_insights.entries;
COMMENT ON VIEW cache_insights.overview IS
  'One row: how big the cache is and how much of it is already expired but still taking space.';

-- ------------------------------------------------------ entries per cache key
CREATE OR REPLACE VIEW cache_insights.entries_per_cache_key AS
SELECT
    cache_key,
    count(*)                                  AS entries,
    count(*) FILTER (WHERE has_embedding)     AS with_embedding,
    count(*) FILTER (WHERE is_expired)        AS expired,
    round(avg(answer_chars))                  AS avg_answer_chars,
    min(stored_at_estimate)                   AS first_stored_estimate,
    max(stored_at_estimate)                   AS last_stored_estimate
FROM cache_insights.entries
GROUP BY cache_key
ORDER BY entries DESC, cache_key;
COMMENT ON VIEW cache_insights.entries_per_cache_key IS
  'x-bf-cache-key is the cache partition (for example one per customer). Biggest first.';

-- ---------------------------------------------------------- oldest entries
CREATE OR REPLACE VIEW cache_insights.oldest_entries AS
SELECT
    cache_id,
    cache_key,
    model,
    stored_at_estimate,
    CASE
        WHEN now() - stored_at_estimate < interval '1 hour' THEN
            round(extract(epoch FROM now() - stored_at_estimate) / 60)::int || ' minutes'
        WHEN now() - stored_at_estimate < interval '2 days' THEN
            round((extract(epoch FROM now() - stored_at_estimate) / 3600)::numeric, 1) || ' hours'
        ELSE
            round((extract(epoch FROM now() - stored_at_estimate) / 86400)::numeric, 1) || ' days'
    END                                        AS age_in_words,
    is_expired,
    answer_chars
FROM cache_insights.entries
ORDER BY stored_at_estimate ASC, cache_id
LIMIT 50;
COMMENT ON VIEW cache_insights.oldest_entries IS 'The 50 oldest entries. No question or answer text, only the answer length.';

-- ----------------------------------------------------------- expiring soon
CREATE OR REPLACE VIEW cache_insights.expiring_soon AS
SELECT
    cache_id,
    cache_key,
    model,
    expires_at,
    round(extract(epoch FROM expires_at - now()) / 60)::int AS minutes_left,
    answer_chars
FROM cache_insights.entries
WHERE NOT is_expired
  AND expires_at < now() + interval '10 minutes'
ORDER BY expires_at;
COMMENT ON VIEW cache_insights.expiring_soon IS 'Entries still valid that will expire within 10 minutes.';

-- ----------------------------------------------------- per model / provider
CREATE OR REPLACE VIEW cache_insights.by_model_provider AS
SELECT
    provider,
    model,
    count(*)                              AS entries,
    count(*) FILTER (WHERE has_embedding) AS with_embedding,
    count(*) FILTER (WHERE is_expired)    AS expired,
    round(avg(answer_chars))              AS avg_answer_chars
FROM cache_insights.entries
GROUP BY provider, model
ORDER BY entries DESC, provider, model;
COMMENT ON VIEW cache_insights.by_model_provider IS 'Entries per provider and model (the cache keeps them apart).';

-- ------------------------------------------------------- stored size estimate
CREATE OR REPLACE VIEW cache_insights.stored_size AS
SELECT
    model,
    count(*)                                   AS entries,
    sum(answer_chars)                          AS answer_chars_total,
    round(avg(metadata_bytes))                 AS avg_metadata_bytes,
    round(avg(metadata_bytes_on_disk))         AS avg_metadata_bytes_on_disk,
    sum(embedding_bytes)                       AS embedding_bytes_total,
    pg_size_pretty(sum(metadata_bytes_on_disk + embedding_bytes)) AS approx_row_data_size,
    pg_size_pretty(COALESCE(sum(metadata_bytes_on_disk + embedding_bytes) FILTER (WHERE is_expired), 0)) AS of_which_expired
FROM cache_insights.entries
GROUP BY model
ORDER BY sum(metadata_bytes_on_disk + embedding_bytes) DESC;
COMMENT ON VIEW cache_insights.stored_size IS
  'Row-data size estimate per model (metadata + vector). On-disk may be smaller than text size because Postgres compresses big values.';

-- ===================================================================== logs
-- Everything below needs the companion logs loaded (sql/load_logs.*). Before
-- that the views simply return no rows.

-- decisions: one row per freshness decision (decisionlog.Record fields)
CREATE OR REPLACE VIEW cache_insights.decisions AS
SELECT
    (doc->>'time')::timestamptz                      AS decided_at,
    doc->>'cache_id'                                 AS cache_id,
    doc->>'hit_type'                                 AS hit_type,         -- direct | semantic
    NULLIF(doc->>'similarity', '')::numeric          AS similarity,       -- semantic hits only
    doc->>'category'                                 AS category,
    doc->>'fact'                                     AS fact,
    doc->>'outcome'                                  AS outcome,          -- confirmed | stale | contradicted ...
    doc->>'action'                                   AS action,           -- served | refreshed | corrected ...
    doc->>'refresh_strategy'                         AS refresh_strategy,
    doc->>'refreshed_cache_id'                       AS refreshed_cache_id,
    doc->>'gate'                                     AS gate,             -- verified | trusted | blocked | unverified
    (doc->>'age_seconds')::numeric                   AS entry_age_seconds,
    (doc->>'latency_ms')::numeric                    AS latency_ms,
    COALESCE((doc->>'shared_refresh')::boolean, false) AS shared_refresh
FROM cache_insights.decisions_raw;

-- audits: one row per wrong-match audit (auditlog.Record fields)
CREATE OR REPLACE VIEW cache_insights.audits AS
SELECT
    doc->>'id'                                       AS audit_id,
    (doc->>'time')::timestamptz                      AS audited_at,
    doc->>'cache_id'                                 AS cache_id,
    (doc->>'similarity')::numeric                    AS similarity,
    (doc->>'threshold')::numeric                     AS threshold,
    COALESCE((doc->>'near_threshold')::boolean, false) AS near_threshold,
    COALESCE((doc->>'gate')::boolean, false)         AS checked_before_serving,
    doc->>'verdict'                                  AS verdict,          -- same | different | error
    doc->>'compare'                                  AS compare_method,   -- exact | judge
    (doc->>'latency_ms')::numeric                    AS latency_ms
FROM cache_insights.audits_raw;

-- Which cache entries were found stale most often?
CREATE OR REPLACE VIEW cache_insights.stale_most_often AS
SELECT
    d.cache_id,
    count(*) FILTER (WHERE d.outcome = 'stale')                      AS times_found_stale,
    count(*)                                                         AS times_checked,
    round(100.0 * count(*) FILTER (WHERE d.outcome = 'stale') / count(*), 1) AS percent_stale,
    max(d.decided_at) FILTER (WHERE d.outcome = 'stale')             AS last_found_stale,
    e.cache_key,
    e.model,
    (e.cache_id IS NOT NULL)                                         AS still_in_cache
FROM cache_insights.decisions d
LEFT JOIN cache_insights.entries e ON e.cache_id = d.cache_id
WHERE d.cache_id IS NOT NULL
GROUP BY d.cache_id, e.cache_id, e.cache_key, e.model
HAVING count(*) FILTER (WHERE d.outcome = 'stale') > 0
ORDER BY times_found_stale DESC, d.cache_id;

-- Which entries did the pre-serve gate block most?  (gate = 'blocked' in the
-- decision log, plus audits that ran before serving and found a different answer.)
CREATE OR REPLACE VIEW cache_insights.blocked_by_gate_most AS
SELECT
    cache_id,
    sum(blocked_decisions)::int       AS blocked_by_decision_log,
    sum(blocked_by_audit)::int        AS blocked_by_gate_audit,
    sum(blocked_decisions + blocked_by_audit)::int AS total_blocks
FROM (
    SELECT cache_id, (gate = 'blocked')::int AS blocked_decisions, 0 AS blocked_by_audit
      FROM cache_insights.decisions WHERE cache_id IS NOT NULL
    UNION ALL
    SELECT cache_id, 0, (checked_before_serving AND verdict = 'different')::int
      FROM cache_insights.audits WHERE cache_id IS NOT NULL
) x
GROUP BY cache_id
HAVING sum(blocked_decisions + blocked_by_audit) > 0
ORDER BY total_blocks DESC, cache_id;

-- Wrong-match rate by similarity bucket (audits that ended 'same' or 'different').
CREATE OR REPLACE VIEW cache_insights.wrong_match_by_similarity AS
SELECT
    to_char(floor(LEAST(similarity, 0.9999) * 20) / 20, 'FM0.00') || ' - ' ||
        to_char(floor(LEAST(similarity, 0.9999) * 20) / 20 + 0.05, 'FM0.00')          AS similarity_bucket,
    count(*)                                                           AS audits,
    count(*) FILTER (WHERE verdict = 'different')                      AS wrong_matches,
    round(100.0 * count(*) FILTER (WHERE verdict = 'different') / count(*), 1) AS wrong_match_percent
FROM cache_insights.audits
WHERE verdict IN ('same', 'different') AND similarity IS NOT NULL
GROUP BY floor(LEAST(similarity, 0.9999) * 20)
ORDER BY floor(LEAST(similarity, 0.9999) * 20);

-- Answers refreshed per day (a stale entry replaced by a fresh answer).
CREATE OR REPLACE VIEW cache_insights.refreshed_per_day AS
SELECT
    (decided_at AT TIME ZONE 'UTC')::date         AS day_utc,
    count(*)                                      AS answers_refreshed,
    count(DISTINCT cache_id)                      AS different_entries
FROM cache_insights.decisions
WHERE action = 'refreshed'
GROUP BY 1
ORDER BY 1;

-- Gate outcomes on risky semantic hits.
CREATE OR REPLACE VIEW cache_insights.gate_outcomes AS
SELECT gate, count(*) AS decisions
FROM cache_insights.decisions
WHERE gate IS NOT NULL
GROUP BY gate
ORDER BY decisions DESC;

-- Entries never hit: in the cache, but no decision and no audit mentions them.
-- (A decision/audit is only written when a request was answered FROM the cache,
-- so this means "stored but never reused as far as the logs show". If the logs are
-- not loaded, every entry shows up here.)
CREATE OR REPLACE VIEW cache_insights.entries_never_hit AS
SELECT
    e.cache_id, e.cache_key, e.model, e.stored_at_estimate, e.is_expired, e.answer_chars
FROM cache_insights.entries e
WHERE NOT EXISTS (SELECT 1 FROM cache_insights.decisions d WHERE d.cache_id = e.cache_id)
  AND NOT EXISTS (SELECT 1 FROM cache_insights.audits a WHERE a.cache_id = e.cache_id)
ORDER BY e.stored_at_estimate, e.cache_id;

-- How many log rows are loaded (a sanity check for the loader).
CREATE OR REPLACE VIEW cache_insights.log_status AS
SELECT
    (SELECT count(*) FROM cache_insights.decisions_raw)  AS decision_rows,
    (SELECT count(*) FROM cache_insights.audits_raw)     AS audit_rows,
    (SELECT count(*) FROM cache_insights.cache_ages_raw) AS cache_age_files,
    (SELECT min(decided_at) FROM cache_insights.decisions) AS first_decision,
    (SELECT max(decided_at) FROM cache_insights.decisions) AS last_decision;
