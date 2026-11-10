# Ask the cache questions in plain SQL

The semantic cache lives in PostgreSQL (table `public."BifrostSemanticCachePlugin"`,
table comment `bifrost_vectorstore`). That means a team can analyse it with ordinary SQL
instead of reading code. This folder adds a small, separate schema called
`cache_insights` with **readable views**. It only reads the cache table; it never
changes or deletes a cache row.

Think of the cache table as a filing cabinet. These views are the index cards: they tell
you how many files there are, how old they are and how much room they take, **without
opening the folders**. No view shows a question or an answer. Answers appear only as a
length (`answer_chars`). (The cache never stores the question text at all.)

## Install (one command)

```powershell
powershell -File sql\install.ps1        # Windows      (or: sh sql/install.sh)
```

Safe to run again. To also load the companion's decision/audit logs (needed for the
"stale / blocked / wrong-match" questions):

```powershell
powershell -File sql\load_logs.ps1      # or: sh sql/load_logs.sh   (a reload REPLACES the old rows)
```

Then open a SQL prompt on the demo database and ask:

```powershell
docker exec -it hackathon-workspace-postgres-1 psql -U bifrost -d bifrost
```

(Other container names: set `CI_POSTGRES` / `CI_COMPANION`. Defaults match `docker-compose.yml`.)

## Remove it

```powershell
powershell -File sql\install.ps1 -Drop   # or: sh sql/install.sh drop   -- same as: DROP SCHEMA cache_insights CASCADE;
```

## What the table really contains (checked on the demo stack)

`id` (text), `embedding` (vector(256), empty for "direct-only" entries), `metadata`
(jsonb with `cache_key`, `provider`, `model`, `params_hash`, `expires_at` in unix
seconds, `stream_chunks`, `response`, `from_bifrost_semantic_cache_plugin`). There is
**no created-at column**, so "age" is an estimate: `expires_at` minus the 30-minute
cache lifetime (`cache_insights.settings`, change it if your ttl differs). If the loader
found `cache-ages.json` in the companion, the exact first-seen time is used instead.
Also, nothing deletes expired rows by itself, so the table keeps growing.

## Example questions and the real output

All outputs below were copied from the running demo stack on 2026-11-06. (The demo
data comes mostly from benchmark runs; the last five rows of the cache are from a few
requests I sent through `http://localhost:8090` with cache keys `sqlinsights-...`.)

**1. How big is the cache and how much of it is already expired?**

```sql
SELECT entries, direct_only_entries, entries_with_embedding, expired_but_still_stored,
       still_valid, total_size, table_bytes, toast_bytes, index_bytes,
       oldest_stored_estimate::timestamp(0), newest_stored_estimate::timestamp(0)
FROM cache_insights.overview;
```
```
 entries | direct_only_entries | entries_with_embedding | expired_but_still_stored | still_valid | total_size | table_bytes | toast_bytes | index_bytes | oldest_stored_estimate | newest_stored_estimate
     286 |                  35 |                    251 |                      281 |           5 | 1064 kB    |      393216 |      393216 |      221184 | 2026-11-04 06:10:31    | 2026-11-06 04:51:31
```

**2. Which cache keys (customers / partitions) hold the most entries?**

```sql
SELECT cache_key, entries, with_embedding, expired
FROM cache_insights.entries_per_cache_key LIMIT 5;
```
```
           cache_key            | entries | with_embedding | expired
--------------------------------+---------+----------------+---------
 impact-plain-1793861692-r1     |      16 |             16 |      16
 impact-companion-1793861692-r1 |      15 |             10 |      15
 impact-companion-1793861847-r1 |      15 |             10 |      15
 impact-companion-1793862128-r1 |      15 |             10 |      15
 impact-companion-1793862128-r3 |      15 |             10 |      15
```

**3. What are the oldest entries? (no text, only size)**

```sql
SELECT cache_key, model, age_in_words, answer_chars FROM cache_insights.oldest_entries LIMIT 3;
```
```
  cache_key   |    model    | age_in_words | answer_chars
--------------+-------------+--------------+--------------
 demo-capital | gpt-4o-mini | 46.7 hours   |         2363
 demo         | gpt-4o-mini | 46.7 hours   |         2391
 demo-price   | gpt-4o-mini | 46.6 hours   |         2391
```

**4. What will expire soon?** (`cache_insights.expiring_soon` lists the next 10 minutes;
this version shows everything still valid.)

```sql
SELECT cache_key, model, minutes_until_expiry
FROM cache_insights.entries WHERE NOT is_expired ORDER BY expires_at LIMIT 5;
```
```
        cache_key         |    model    | minutes_until_expiry
--------------------------+-------------+----------------------
 sqlinsights-1793960490-a | gpt-4o-mini |                   27
 sqlinsights-1793960490-a | gpt-4o-mini |                   27
 sqlinsights-1793960490-b | gpt-4o-mini |                   27
 sqlinsights-1793960490-c | gpt-4o-mini |                   27
 sqlinsights-1793960490-c | gpt-4o-mini |                   27
```

**5. Entries per provider and model**

```sql
SELECT * FROM cache_insights.by_model_provider;
```
```
 provider |    model    | entries | with_embedding | expired | avg_answer_chars
----------+-------------+---------+----------------+---------+------------------
 openai   | gpt-4o-mini |     271 |            240 |     266 |             2386
 openai   | mock-chat   |      15 |             11 |      15 |             2378
```

**6. How much room do the entries take, and how much of it is dead weight?**

```sql
SELECT model, entries, avg_metadata_bytes_on_disk, embedding_bytes_total,
       approx_row_data_size, of_which_expired
FROM cache_insights.stored_size;
```
```
    model    | entries | avg_metadata_bytes_on_disk | embedding_bytes_total | approx_row_data_size | of_which_expired
-------------+---------+----------------------------+-----------------------+----------------------+------------------
 gpt-4o-mini |     271 |                       1275 |                247680 | 579 kB               | 569 kB
 mock-chat   |      15 |                       1264 |                 11352 | 30 kB                | 30 kB
```

**7. Which entries were found stale most often?** (needs the logs loaded)

```sql
SELECT cache_id, times_found_stale, times_checked, percent_stale, cache_key
FROM cache_insights.stale_most_often LIMIT 3;
```
```
               cache_id               | times_found_stale | times_checked | percent_stale |           cache_key
--------------------------------------+-------------------+---------------+---------------+--------------------------------
 236db2e3-68ac-5522-acda-bdfc423edc27 |                17 |            19 |          89.5 | impact-companion-1793862128-r1
 29aa1eb0-72a5-5726-b5f8-0952419bd5c9 |                17 |            19 |          89.5 | impact-companion-1793861847-r1
 3dedb043-9ae2-5200-9309-502e619ed7b9 |                17 |            19 |          89.5 | impact-companion-1793862128-r3
```

**8. How often is a similarity-based match wrong, by similarity score?**
(`audits` = the companion asked the model again and compared.)

```sql
SELECT * FROM cache_insights.wrong_match_by_similarity;
```
```
 similarity_bucket | audits | wrong_matches | wrong_match_percent
-------------------+--------+---------------+---------------------
 0.80 - 0.85       |     55 |            23 |                41.8
 0.85 - 0.90       |     14 |             0 |                 0.0
 0.90 - 0.95       |     15 |             0 |                 0.0
 0.95 - 1.00       |     37 |            16 |                43.2
```
The demo traffic was built to contain look-alike questions, so these rates describe the
test data, not real users. The 0.95 - 1.00 bucket shows that a high
score alone did not guarantee a right answer in this data.

**9. How many stale answers were refreshed each day?**

```sql
SELECT * FROM cache_insights.refreshed_per_day;
```
```
  day_utc   | answers_refreshed | different_entries
------------+-------------------+-------------------
 2026-11-04 |                 4 |                 3
 2026-11-05 |               138 |                27
```

**10. Which entries did the gate block most?** (`blocked_by_gate_audit` = an audit that
ran before serving found a different answer; the decision log's own `gate = 'blocked'`
count is 0 in this data.)

```sql
SELECT * FROM cache_insights.blocked_by_gate_most LIMIT 3;
```
```
               cache_id               | blocked_by_decision_log | blocked_by_gate_audit | total_blocks
--------------------------------------+-------------------------+-----------------------+--------------
 27c756e8-e797-59b1-949e-5ca494f6bef1 |                       0 |                     2 |            2
 2da541d6-3366-5a30-a578-4277f09df2b9 |                       0 |                     2 |            2
 0779c70f-8f25-5cd8-8b15-8030d2c25821 |                       0 |                     1 |            1
```

**11. Entries that were stored but never reused?**

```sql
SELECT count(*) AS never_hit, count(*) FILTER (WHERE is_expired) AS of_which_expired
FROM cache_insights.entries_never_hit;
```
```
 never_hit | of_which_expired
-----------+------------------
       212 |              208
```
(Careful: this only means "no decision or audit record mentions it". The companion logs
decisions for freshness-checked price facts, so plain hits that were never checked also
count as "never hit".)

## All views

`entries` (base), `overview`, `entries_per_cache_key`, `oldest_entries`, `expiring_soon`,
`by_model_provider`, `stored_size`, and from the logs: `decisions`, `audits`,
`stale_most_often`, `blocked_by_gate_most`, `wrong_match_by_similarity`,
`refreshed_per_day`, `gate_outcomes`, `entries_never_hit`, `log_status`.
Staging tables: `decisions_raw`, `audits_raw`, `cache_ages_raw` (one JSON document per row).
Run `\dv cache_insights.*` to list them; each has a comment (`\dv+`).

## How the log loading works

The companion writes JSON Lines to `/app/data/decisions.jsonl` and `audits.jsonl`
(plus `cache-ages.json`). `load_logs.*` runs `docker cp` to copy them out, then a normal
PostgreSQL `COPY` puts each line into a `jsonb` column inside one transaction
(truncate + copy), so reloading never duplicates. No new driver is added to the companion.
The logs hold only metadata and scores, never prompts or answers.

## Test

```powershell
powershell -File sql\test.ps1
```
Installs twice, reloads logs twice, runs every view, and checks the numbers against
independent queries (for example `overview.entries` equals `COUNT(*)` of the table, the
log tables equal the line counts of the files in the container, buckets add up to the
audits in the file). It also checks that no view has a prompt/answer column, that
dropping the schema is clean, and that the cache table (row count and an md5 of every
row) is identical before and after. Last run: 45 passed, 0 failed.
