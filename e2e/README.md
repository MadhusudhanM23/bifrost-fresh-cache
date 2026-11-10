# End-to-end proof: Bifrost semantic cache on PostgreSQL + pgvector (no paid keys)

This directory drives the **real `bifrost-http` gateway**, built from the local fork
(`../bifrost`, branch `feat/pgvector-vectorstore`, uncommitted pgvector backend), through
the **semantic cache plugin** into the **`pgvector` vector store** on the local test
database. The OpenAI provider points at a local mock (`../tools/mock-openai`), so no real
API key is used anywhere; the only "keys" are mock values (`sk-mock-local`,
`sk-bf-e2e-probe-local`, `e2e-setup-token-local`).

## What is proven

Every claim below comes from a run of `go run .` in this directory; the full console
output is in `e2e-run.log` (process logs in `logs/`).

| Step | What was checked | Observed in the run |
|---|---|---|
| 0 | Gateway starts with `vector_store.type = pgvector`; the plugin's `CreateNamespace` creates the table | Table `public."BifrostSemanticCachePlugin"` (`id text`, `embedding vector(256)`, `metadata jsonb`), comment `bifrost_vectorstore`, indexes `..._emb_hnsw` (hnsw, `vector_cosine_ops`) and `..._meta_gin` (gin, `jsonb_path_ops`); log `plugin status: semantic_cache - active` |
| a | First request with `x-bf-cache-key: demo` | `x-bifrost-cache-hit: false`, `x-bifrost-cache-id: e9480d9f-3b85-504b-b6b4-976fbb46514d`, provider/model used `openai`/`mock-embedding`, input tokens 8; mock `chat_calls=1`, `embedding_calls=1`; 1 row in Postgres with that id, a 256-dim embedding and the stored completion `chatcmpl-mock-000001` |
| b | Identical request | `hit: true`, `hit-type: direct`, same cache id, hit latency 1-8 ms; `chat_calls` still 1, **no embedding call** (the direct lookup runs first) |
| c | Paraphrase `Tell me the capital city of France.` | `hit: true`, `hit-type: semantic`, `similarity: 0.878310110518638`, `threshold: 0.8`; `chat_calls` still 1; the similarity computed by pgvector equals the mock's own float64 cosine of the two embedded texts (0.878310066) within 1e-5 |
| d | Unrelated request | `hit: false`; `chat_calls` 1 -> 2; its cosine to the cached entry is -0.0321; a second row is written |
| e | Same question as (a) with `x-bf-cache-key: other` | `hit: false`, different cache id (`613f9443-...`): the key is part of the direct id and a filter of the semantic search; `chat_calls` 2 -> 3 |
| f | `DELETE /api/cache/clear/{id from a}` | **No credential needed in this setup** (see "Clearing and auth"); 200 `{"message":"Cache cleared successfully"}`; the row disappears from Postgres; the same request then misses, `chat_calls` 3 -> 4, and the fresh completion is written back **under the same id**; `DELETE /api/cache/clear-by-key/other` -> 200, 0 rows left under `other`; a request carrying `Origin: https://evil.example.com` is refused with 403 |
| g | Bypass / refresh candidates | See "Bypassing or refreshing one entry" |
| h | Kill and restart the gateway | Rows unchanged while it is down; `CreateNamespace` accepts the existing table; identical request -> direct hit serving the refreshed completion; a new paraphrase -> semantic hit (similarity 0.8764167093162752) from the pgvector cosine search (seq scan at this size, see x6); `chat_calls` unchanged |
| i | Where the metadata is in the body | `extra_fields.cache_debug` (see below), identical to the headers |
| x1 | `x-bf-cache-type: direct` | Row written with `embedding IS NULL` (pgvector `RequiresVectors() == false`), no embedding call; later direct hit works; a paraphrase (mock cosine 0.8876) misses because `GetNearest` only considers rows with an embedding |
| x2 | `x-bf-cache-ttl: 2s` | `expires_at` stored as a JSON number about 2 s ahead; hit before expiry; miss after; the expired row is deleted and rewritten with the default 30 min TTL |
| x3 | Streaming (`stream: true`) | 9 chunks stored as a JSONB string array in `stream_chunks` (no `response` field); the second stream is a direct hit with identical content and id, and the final chunk carries `extra_fields.cache_debug` |
| x4 | OpenAI SDK route `/openai/v1/chat/completions` | Same cache headers; the body also carries `extra_fields` (direct hit on the entry from (a)) |
| x5 | Auth when a config store **is** enabled | Inference without `x-bf-vk` -> 401 `virtual_key_required`; `DELETE /api/cache/clear/{id}` without credential -> 401, wrong `X-Bifrost-Setup-Token` -> 403, correct token -> 200 |

## What is not proven (limitations)

- **The mock embeddings are not semantic.** They are bag-of-words feature hashing (see
  `../tools/mock-openai/README.md`). The run proves the mechanics (embedding request, vector
  storage, HNSW nearest-neighbour search, threshold, filters, TTL, deletes, persistence),
  not cache quality. "Summarise" vs "Summarize the sales report for the quarter" scores
  0.70 with the mock and would miss at 0.8; a real model would behave differently in both
  directions. Do not quote hit rates measured with it.
- Only chat completions (non-streaming and streaming) were exercised, against the
  `openai` provider. Responses API, embeddings, speech, images and other providers were
  not run through the cache here.
- One gateway at a time, low concurrency, a handful of rows. No load, no multi-replica
  test, no large table (HNSW behaviour at scale, `iterative_scan` under a selective filter
  with many rows, index build time are untested here).
- The gateway is a native Windows build with `CGO_ENABLED=0` and the main run has **no
  config store and no log store** (SQLite needs CGO). That is not how most deployments run;
  x5 covers the config-store case for auth only.
- The UI is a placeholder (the dashboard was not built).
- `transports/config.schema.json` in the fork did not list `pgvector` when the binary was
  first built, so IDE validation against the published schema flags `vector_store.type`.
  The gateway does not validate config.json against the schema at runtime, so it works.

## Files

| Path | Purpose |
|---|---|
| `config.json` | Gateway config for the main run (this directory is the gateway's `-app-dir`) |
| `authprobe/config.json` | Config for x5: Postgres config store in a scratch database, `setup_token`, one virtual key, separate namespace |
| `main.go`, `harness.go`, `steps.go`, `go.mod` | The e2e runner (Go standard library only) |
| `run-e2e.ps1` | Convenience wrapper: builds missing binaries with the commands below, then `go run .` |
| `build/go.work` | Out-of-tree Go workspace used to build the gateway from the fork (keeps the bifrost repo untouched) |
| `bin/bifrost-http.exe`, `bin/mock-openai.exe` | Built binaries |
| `e2e-run.log` | Console output of the last run |
| `probes/` | `latency.sh`, `expiry_race.sh` (need a running mock + gateway), `pgrtt/` (pgx round-trip timing: `go run .`) |
| `logs/` | `mock-openai.log`, `gateway-run1.log`, `gateway-run2.log` (after the restart), `gateway-authprobe.log` |

## Reproduce

Prerequisites: Go 1.27.3 at `C:\Program Files\Go\bin` (not on PATH in new shells), Docker
Desktop, and the pgvector test container running:

```powershell
docker ps --filter name=bifrost-pgvector-test   # pgvector/pgvector:pg16 on localhost:5433
```

### 1. Build the mock (and run its tests)

```powershell
$env:Path = "C:\Program Files\Go\bin;" + $env:Path
cd C:\Madhusudhan\Geeks\hackathon-workspace\tools\mock-openai
go test -count=1 ./...
$env:CGO_ENABLED = "0"; go build -o ..\..\e2e\bin\mock-openai.exe .
```

### 2. Build the gateway from the fork (native Windows, CGO disabled)

This is the method that worked. Two things matter:

- `//go:embed all:ui` in `transports/bifrost-http/main.go` needs a `ui` directory. A
  one-file placeholder is enough; the path is gitignored (`/transports/bifrost-http/ui/`
  in `.gitignore`), so it never shows up in `git status`.
- The gateway must be built in **workspace mode** against the local modules. The Makefile
  builds with `GOWORK=off` unless `LOCAL=1`, which would pull the published `framework`
  module without pgvector. The repo has no `go.work` (it is gitignored and created by
  `make setup-workspace`), and creating one there would change how other engineers' builds
  resolve modules, so this uses `build/go.work` outside the repo through `GOWORK`.
  Never run `go work sync` with it: that rewrites the bifrost modules' `go.mod` files.

```powershell
$env:Path = "C:\Program Files\Go\bin;" + $env:Path
$ui = "C:\Madhusudhan\Geeks\hackathon-workspace\bifrost\transports\bifrost-http\ui"
New-Item -ItemType Directory -Force $ui | Out-Null
'<!doctype html><html><head><meta charset="utf-8"><title>Bifrost (placeholder UI)</title></head><body><p>Placeholder UI for a local e2e build.</p></body></html>' | Out-File -Encoding utf8 "$ui\index.html"

$env:GOWORK = "C:\Madhusudhan\Geeks\hackathon-workspace\e2e\build\go.work"
$env:CGO_ENABLED = "0"
cd C:\Madhusudhan\Geeks\hackathon-workspace\bifrost\transports\bifrost-http
go build -ldflags="-X main.Version=v0.0.0-e2e-pgvector" -o C:\Madhusudhan\Geeks\hackathon-workspace\e2e\bin\bifrost-http.exe .
Remove-Item Env:\GOWORK
```

First build about 3 min (module downloads), rebuild about 2.5 min. Check that the binary
was linked against the local modules: `go version -m bin\bifrost-http.exe` lists every
`github.com/maximhq/bifrost/...` module as `(devel)` and `CGO_ENABLED=0`.

With `CGO_ENABLED=0`, `mattn/go-sqlite3` compiles to a stub that fails at runtime, so the
config sets `config_store.enabled = false` and `logs_store.enabled = false` (otherwise both
default to SQLite files in the app dir). A Docker build was not needed.

### 3. Run the e2e

```powershell
$env:Path = "C:\Program Files\Go\bin;" + $env:Path
cd C:\Madhusudhan\Geeks\hackathon-workspace\e2e
go run .            # or: .\run-e2e.ps1
```

The runner checks that ports 18080 and 18090 (and 18091 for x5) are free, drops and lets
the gateway recreate the namespace table (`-fresh=true`, default), starts the mock and the
gateway itself (it needs to restart the gateway in step h), prints PASS/FAIL per step and a
summary, writes `e2e-run.log`, and stops every process it started. Useful flags:
`-extras=false` (skip x1-x4), `-authprobe=false` (skip x5, which creates and drops the
scratch database `bifrost_e2e_configstore` in the container), `-keep` (leave the mock and
gateway running), `-mock-chat-latency 100ms` (default), `-gateway-port`, `-mock-port`.

Manual inspection while it runs (or with `-keep`):

```powershell
docker exec bifrost-pgvector-test psql -U bifrost -d bifrost -c 'SELECT id, metadata->>''cache_key'' AS key, embedding IS NULL AS no_vec FROM "BifrostSemanticCachePlugin"'
curl.exe -s http://127.0.0.1:18080/mock/stats
```

## Config (`config.json`)

```json
{
  "config_store": { "enabled": false },
  "logs_store":   { "enabled": false },
  "providers": {
    "openai": {
      "keys": [{ "name": "mock-openai-local", "value": "sk-mock-local", "models": ["*"], "weight": 1.0 }],
      "network_config": { "base_url": "http://127.0.0.1:18080", "default_request_timeout_in_seconds": 30, "max_retries": 0 }
    }
  },
  "vector_store": {
    "enabled": true,
    "type": "pgvector",
    "config": { "host": "127.0.0.1", "port": "5433", "user": "bifrost", "password": "bifrost_password",
                "database": "bifrost", "ssl_mode": "disable", "schema": "public", "max_conns": "5" }
  },
  "plugins": [{
    "enabled": true, "name": "semantic_cache",
    "config": { "provider": "openai", "embedding_model": "mock-embedding", "dimension": 256,
                "threshold": 0.8, "ttl": "30m", "conversation_history_threshold": 3,
                "cache_by_model": true, "cache_by_provider": true, "exclude_system_prompt": false }
  }]
}
```

(The real file also has `$schema` and a small `client` block.) Notes:

- `base_url` is the mock's origin; the OpenAI provider appends `/v1/chat/completions` and
  `/v1/embeddings`. Loopback is always allowed. From a container you would use
  `http://host.docker.internal:18080`, start the mock with `-host 0.0.0.0`, and set
  `"allow_private_network": true` in `network_config`.
- `"models": ["*"]` is required: an empty list denies every model.
- `dimension` must equal the mock's `-dim` (the runner passes the config value to the mock).
- `vector_store_namespace` is omitted on purpose, so the plugin default
  `BifrostSemanticCachePlugin` is exercised. It is a mixed-case name, so the table is a
  quoted identifier: write `"BifrostSemanticCachePlugin"` in psql.
- pgvector `port` and `max_conns` are `SecretVar` strings; a JSON number also works. Note
  that a non-numeric value silently falls back to the default (5432 / 10).
- `ttl` is 30 min so nothing expires during the run except where x2 sets its own TTL.

## Observed values from a real run

Headers on the first request (a), a miss:

```
x-bifrost-cache-hit: false
x-bifrost-cache-id: e9480d9f-3b85-504b-b6b4-976fbb46514d
x-bifrost-cache-provider-used: openai
x-bifrost-cache-model-used: mock-embedding
x-bifrost-cache-input-tokens: 8
```

Direct hit (b):

```
x-bifrost-cache-hit: true
x-bifrost-cache-id: e9480d9f-3b85-504b-b6b4-976fbb46514d
x-bifrost-cache-hit-type: direct
x-bifrost-cache-hit-latency-ms: 1
x-bifrost-cache-requested-provider: openai
x-bifrost-cache-requested-model: mock-chat
```

Semantic hit (c):

```
x-bifrost-cache-hit: true
x-bifrost-cache-id: e9480d9f-3b85-504b-b6b4-976fbb46514d
x-bifrost-cache-hit-type: semantic
x-bifrost-cache-similarity: 0.878310110518638
x-bifrost-cache-threshold: 0.8
x-bifrost-cache-hit-latency-ms: 68
x-bifrost-cache-provider-used: openai
x-bifrost-cache-model-used: mock-embedding
x-bifrost-cache-input-tokens: 8
x-bifrost-cache-requested-provider: openai
x-bifrost-cache-requested-model: mock-chat
```

The cache ids of direct entries are deterministic UUIDv5 values (cache key, provider,
model, normalised input hash, params hash), so the same ids appear on every fresh run.
The row written in (a), as shown by psql:

```
 id                                   | cache_key | provider | model     | params_hash      | embedding   | expires_in_s | stream_chunks | cached_response_id
 e9480d9f-3b85-504b-b6b4-976fbb46514d | demo      | openai   | mock-chat | ccb9da55fe35fa4a | vector(256) |         1799 |             0 | chatcmpl-mock-000001
```

Text the gateway sent to `/v1/embeddings` for the base question:
`"user: what is the capital city of france?"` (role prefix, lowercased, trimmed; with a
system prompt it would be one `role: content` line per message).

## Cache metadata in the JSON body

On `/v1/chat/completions` (and on the OpenAI SDK route `/openai/v1/chat/completions`) the
body carries the same data under **`extra_fields.cache_debug`**:

```json
"extra_fields": {
  "cache_debug": {
    "cache_hit": true,
    "cache_id": "e9480d9f-3b85-504b-b6b4-976fbb46514d",
    "hit_type": "semantic",
    "requested_provider": "openai",
    "requested_model": "mock-chat",
    "provider_used": "openai",
    "model_used": "mock-embedding",
    "input_tokens": 8,
    "threshold": 0.8,
    "similarity": 0.878310110518638,
    "cache_hit_latency": 68
  }
}
```

A miss carries `cache_hit: false`, `cache_id` (the id the response is written under) and,
when a semantic lookup ran, `provider_used`, `model_used`, `input_tokens`. A direct hit has
no embedding fields. For streams, `cache_debug` is only on the final chunk; the headers are
sent before the first chunk. Header names map 1:1 (`x-bifrost-cache-hit-latency-ms` <->
`cache_hit_latency`). Note the cached response body is the original upstream response, so
`id`, `created` and `usage` are those of the first call.

## Clearing and auth

- **This setup (`config_store.enabled = false`)**: the server installs no auth middleware
  (log: `auth middleware requires config store, skipping auth middleware initialization`).
  `/api/*` routes get `AuthBypassedMiddleware`, which checks **no credential at all**. The
  only refusal is for browser-style requests whose `Origin` is foreign (not same-origin,
  not localhost, not in `client.allowed_origins`): 403. `curl`/SDK calls send no `Origin`
  and pass. So `DELETE /api/cache/clear/{cacheId}` and `/api/cache/clear-by-key/{key}`
  needed nothing.
- **With a config store (OSS, x5)**: while no admin account exists or dashboard auth is
  off, `SetupLockMiddleware` locks `/api/*` behind the operator's setup token
  (`setup_token` in config.json or `BIFROST_SETUP_TOKEN`). Observed: no header -> 401,
  wrong `X-Bifrost-Setup-Token` -> 403, correct token -> 200. Without a configured setup
  token every non-public `/api` call is 403. Once dashboard auth is enabled, the normal
  Basic/Bearer session auth applies instead and the setup token stops working.
- **Inference auth differs too**: with a fresh config store, `enforce_auth_on_inference`
  resolves to true and inference without `x-bf-vk` returned 401 `virtual_key_required`.
  In the main run (no config store) inference was open: the governance plugin failed to
  initialise (`governance config is nil`), so nothing enforced a virtual key.
- `clear/{cacheId}` deletes one row by id (`DELETE ... WHERE id = $1`), succeeds for an id
  that does not exist, and returns 405 for an empty id. `clear-by-key/{key}` deletes every
  row whose `metadata.cache_key` matches (across identities) and is URL-decoded first.

## Bypassing or refreshing one entry

| Method | Does the request read the cache? | What is written afterwards | Verdict |
|---|---|---|---|
| `DELETE /api/cache/clear/{cacheId}`, then repeat the request | Misses (entry gone) | The fresh response is written under the **same** deterministic id; the next identical request serves it (`chatcmpl-mock-000004` replaced `chatcmpl-mock-000001`) | **Most reliable refresh** |
| Omit `x-bf-cache-key` | No lookup at all, no `x-bifrost-cache-*` headers | Nothing | **Reliable bypass** (no read, no write), but only while no `default_cache_key` is configured; with one, omitting the header still caches |
| `x-bf-cache-type: semantic` + `x-bf-cache-threshold: 1.5` | The header is clamped to 1.0. An **identical** prompt is still served (semantic hit, similarity 1: identical embedding, cosine distance 0). A paraphrase misses | On a miss, a **second** row is added under the request id (with embedding); the original entry stays. Later normal paraphrases are then served from that new row (similarity 1) | Not a bypass for identical prompts; creates duplicate entries |
| `x-bf-cache-no-store: true` | Yes: a cached hit is still served | Nothing on a miss | Not a bypass; useful to avoid writing |
| Different `x-bf-cache-key` | Misses | A new entry under that key | Works as a bypass but pollutes the cache |

The per-request threshold can only raise the configured threshold, never lower it.

## Findings and surprises

1. **No pgvector backend bug was found** in the runs. Similarity scores from pgvector match
   independent float64 cosines within float32 precision; NULL embeddings, JSONB arrays
   (`stream_chunks`), JSON-number `expires_at`, deletes by id and by key, persistence across
   a kill/restart, and the mixed-case default namespace all behaved as the plugin expects.
2. **The fork changed under the run.** `framework/vectorstore/pgvector.go` was edited by
   another engineer at 16:49 (binary pgvector codec registered in `AfterConnect`, `$n::vector`
   binds instead of text literals). The first runs used a binary built at 16:46 from the
   previous version; the gateway was then rebuilt and the full suite re-run. The run header
   of `e2e-run.log` prints the sha256 of `pgvector.go` at run time.
3. **Semantic-hit latency comes from round trips, not Postgres.** Direct hits take 1-8 ms. Semantic
   hits took 5-8 ms when idle but spiked to 16-76 ms during bursts. Inside Postgres the query
   runs in 0.36 ms. `GetNearest` makes 4 round trips (BEGIN, SET LOCAL hnsw.iterative_scan,
   SELECT, ROLLBACK). Over the Windows-to-Docker-Desktop link, `probes/pgrtt` measured a
   single SELECT at p50 1.0-1.5 ms. The 4-round-trip pattern measured p50 3.1-9.4 ms and
   p99 34-124 ms. Suggestion: set `hnsw.iterative_scan` once per connection in
   `AfterConnect`, or pipeline the statements, so a search costs one round trip.
3b. **The HNSW index is created but not used at e2e sizes.** With 8-20 rows the planner picks
   a Seq Scan. x6 forces `enable_seqscan=off, enable_bitmapscan=off`. With that, the plan is
   `Index Scan using "BifrostSemanticCachePlugin_emb_hnsw"` and it returns the same row. The
   HNSW + iterative-scan path at real table sizes is still untested end to end.
3c. **Expired-entry delete vs rewrite.** The plugin deletes an expired entry asynchronously
   while it also writes the fresh response under the same id. That ordering could race. It
   was not observed: in `probes/expiry_race.sh`, direct mode with 0 ms upstream latency,
   0 of 20 rows were lost.
4. In no-config-store mode the governance plugin reports `error` at startup and inference
   is unauthenticated; with a config store the defaults are much stricter (VK required,
   setup-token lock on `/api`). Plan the demo config accordingly.
5. Bifrost embeds `role: content` per message, including the system prompt unless
   `exclude_system_prompt` is true. With a long shared system prompt (RAG context), unrelated
   questions can become similar; keep this in mind with real embeddings too.

## Ports and processes

Mock `127.0.0.1:18080`, gateway `127.0.0.1:18090`, auth-probe gateway `127.0.0.1:18091`,
Postgres `localhost:5433` (container `bifrost-pgvector-test`, left running). The runner
prints every PID it started and confirms at the end that the three ports are closed.
