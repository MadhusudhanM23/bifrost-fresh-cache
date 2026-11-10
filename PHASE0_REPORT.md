# Phase 0 report: Bifrost PGVector semantic cache

Date: 2 Nov 2026. Read-only inspection plus environment setup. No Bifrost source has been modified.

## 1. Reproducibility record

| Field | Value |
|---|---|
| Repository | https://github.com/maximhq/bifrost (cloned to `hackathon-workspace/bifrost`) |
| Branch / commit | `dev` / `3ffcfd965c8bd8e4559453c3d4d2d21f8517b944` (2026-11-02 15:59 +05:30) |
| OS | Windows 11 Pro 10.0.26300 |
| Go | 1.27.3 windows/amd64 (installed via winget). `go.work` and module `go.mod` files require exactly 1.27.3. |
| Docker | 29.7.2 (Docker Desktop) |
| WSL2 | present (default distro is `docker-desktop`) |
| PostgreSQL / pgvector | not yet chosen. Test compose file uses `postgres:16-alpine` (no pgvector). Plan: `pgvector/pgvector:pg16`. |
| Embedding model / dimension | not chosen yet |
| Tavily | not tested yet (needs an API key from the user) |

## 2. Duplicate check (repository side)

- `framework/vectorstore` contains chromem, pinecone, qdrant, redis, weaviate. No Postgres or pgvector store.
- A repo-wide case-insensitive search for pgvector finds only one unrelated test file (`framework/configstore/complexityconfig_test.go`).
- Issue #3180 was open, unassigned, with no comments and no linked PR when checked signed out. Re-check signed in.

## 3. The interface (exact, from `framework/vectorstore/store.go`)

13 methods: `Ping`, `CreateNamespace(ns, dimension, properties)`, `DeleteNamespace`, `ListNamespaces(prefix)`, `GetChunk`, `GetChunks`, `GetAll(ns, queries, selectFields, cursor, limit)`, `GetNearest(ns, vector, queries, selectFields, threshold, limit)`, `RequiresVectors`, `Add(ns, id, embedding, metadata)`, `Delete`, `DeleteAll(ns, queries)`, `Close(ns)`.

Key semantics:
- `GetNearest`: `threshold` and each result's `Score` are **cosine similarity on every store**.
- Query operators to translate: Equal, NotEqual, GreaterThan, LessThan, GreaterThanOrEqual, LessThanOrEqual, Like, ContainsAny, ContainsAll, IsNull, IsNotNull.
- Property types: string, integer, boolean, string[]. Properties can be marked `Filterable`.
- Optional hooks: `WithDisableScanFallback(ctx)`, `WithIncludeVectors(ctx)` (GetAll returns stored vectors), optional `ServerFilteredReader` interface.
- Registration: type constant, typed config struct decoded in `Config.UnmarshalJSON`, and a case in `NewVectorStore`.

## 4. Which methods the semantic cache actually calls

Non-test code in `plugins/semanticcache`:

| Method | Call sites |
|---|---|
| `CreateNamespace` | `main.go:324` (at plugin init, with the configured dimension) |
| `Add` | `utils.go:583`, `stream.go:138` |
| `GetChunk` | `search.go:38` (direct cache id lookup) |
| `GetNearest` | `search.go:93` (limit 1; filters below) |
| `Delete` | `search.go:261`, `main.go:1065` |
| `DeleteAll` | `main.go:1038` (cache clear by key or similar) |
| `RequiresVectors` | `main.go:634, 791, 927, 933` |

`GetNearest` strict filters (all Equal): `cache_key`, `params_hash`, `from_bifrost_semantic_cache_plugin` (bool), optionally `provider` and `model`. Select fields: `response`, `stream_chunks`, `expires_at`, `cache_key`, `provider`, `model`. Expiry lives in an `expires_at` property.

Not called by the semantic cache: `Ping`, `GetChunks`, `GetAll`, `ListNamespaces`, `DeleteNamespace`, `Close`.
Other consumers of the same interface: `framework/warp` (uses `GetAll`, `ListNamespaces`, `DeleteNamespace`), so those methods matter for upstream completeness. The contributor guide asks to implement all methods.

## 5. Spike A answers from source (cache hit, bypass)

- **Hit identification is supported.** Responses carry `cache_debug` (`BifrostCacheMetadata` in `core/schemas/bifrost.go`): `cache_hit`, `cache_id`, `hit_type`, `threshold`, `similarity`, `cache_hit_latency`, plus provider/model/input tokens. The plugin also sets `x-bifrost-cache-*` response headers with the same fields (exact header names still to confirm in the transport code).
- **Per-request controls exist** (request headers): `x-bf-cache-key`, `x-bf-cache-ttl`, `x-bf-cache-threshold`, `x-bf-cache-type` (direct or semantic), `x-bf-cache-no-store` (skips writing; still serves hits).
- **Bypass:** caching is disabled when no cache key is set (no header and no `default_cache_key`). A re-request without the key, or with a different key, should skip the cached entries. To be confirmed by a live test. Raising the threshold above 1 is another candidate.
- **Entry age:** not in `cache_debug`. It can be derived from `expires_at` minus the TTL, or tracked by the companion.
- **Implication for extras:** explain-a-hit data (similarity, threshold, hit type) is already exposed by Bifrost, so E5 would add little. Freshness (E4) is viable at the companion layer through headers and `cache_debug`.

## 6. Registration touchpoints beyond `store.go`

Files that mention an existing store type and will likely need a pgvector entry (verify each): `transports/config.schema.json` (source of truth for config.json), `transports/bifrost-http/lib/config.go` (Weaviate has special handling near line 9992) and `validator_test.go`, `plugins/semanticcache/test_utils.go` and `main_test.go`, `tests/docker-compose.yml`, `helm-charts/bifrost/values.yaml` and `values.schema.json`, `terraform/modules/bifrost/variables.tf`, docs under `docs/integrations/vector-databases/`, and possibly the UI settings. Follow the guide at `docs/contributing/adding-a-vectorstore.mdx`: file `framework/vectorstore/pgvector.go`, names `VectorStoreType...`, `...Config`, `...Store`, `new...Store`.

## 7. Dependencies

`github.com/jackc/pgx/v5` v5.9.2 and `gorm.io/driver/postgres` are already in `framework/go.mod`. The contributor guide says to use the native client, so pgx is the natural choice. `pgvector-go` would be a new dependency; sending vectors as text literals (`'[1,2,3]'::vector`) with pgx avoids it. Decide after prototype.

## 8. Contribution requirements found in `AGENTS.md`

- Multi-module Go workspace; run `go mod tidy` in the specific module.
- Framework tests need backing services: `docker compose -f tests/docker-compose.yml up -d`, then `make test-framework`.
- Request-path code needs a `Benchmark*` with `-benchmem`, and the PR body records allocs/op and ns/op before and after. A change that cannot be benchmarked must say why. Reviewers treat a missing cost statement like missing tests.
- Naming and file conventions in section 6.

## 9. Open items / blockers

1. Confirm pgvector behaviour for namespace design: the plugin uses one namespace (config `VectorStoreNamespace`) with a fixed `dimension`, so a table-per-namespace design maps cleanly. Decide after reading `qdrant.go` or `redis.go` end to end.
2. Tavily capability test on a public page (needs the user's API key).
3. Organizer confirmation about a rebuilt Bifrost, Tavily usage for Problem 04, submission fields, deadline time zone. Messages are drafted in the blueprint; the user sends them.
4. Team size and Go experience (affects scope).

## 10. Recommendation: GO

Proceed to the Day-3 vertical slice: `pgvector.go` with config, constructor, `Ping`, `CreateNamespace`, `Add`, `GetNearest`, wired into `store.go`, tested against a `pgvector/pgvector` container. Then extend to the remaining methods and tests.

## 11. Day-3 gate result (reached on 2 Nov, day 1)

Branch `feat/pgvector-vectorstore` in the local fork. **Not committed** (waiting for the user).

| Check | Result |
|---|---|
| `go build ./vectorstore/` | pass |
| `go vet ./vectorstore/` (incl. tests) | pass |
| `gofmt` (on LF-normalised copies; the Windows checkout uses CRLF via `core.autocrlf=true`, which makes gofmt flag every file, even untouched ones) | clean |
| `go test ./vectorstore/ -run PgVector -count=1 -v` against `pgvector/pgvector:pg16` (PostgreSQL 16.15, pgvector 0.8.7) | **27 tests pass** (incl. 18 filter sub-cases), 2.8 s |

Files: `framework/vectorstore/pgvector.go` (new), `framework/vectorstore/pgvector_test.go` (new), `framework/vectorstore/store.go` (+16 lines: type constant, JSON parse case, factory case).

Design as built:
- One table per namespace: `id TEXT PRIMARY KEY, embedding vector(dim) NULL, metadata JSONB`. Tables are marked with a comment; the store never adopts or drops a table it did not create.
- `RequiresVectors()` = false (nullable embedding), so the semantic cache stores direct-hash and embedding-request entries without placeholder vectors.
- Indexes: GIN (`jsonb_path_ops`) on metadata for the Equal filters the cache sends; HNSW (`vector_cosine_ops`) when dimension <= 2000, otherwise a warning and exact search.
- Similarity = 1 - cosine distance; threshold applied as distance <= 1 - threshold; scores clamped to [-1, 1].
- All filter values **and field names** are bind parameters; namespaces/schema validated against `^[A-Za-z_][A-Za-z0-9_]*$` (namespace max 48 chars) and quoted.
- Concurrent `CreateNamespace` serialised with a transaction-scoped advisory lock (tested with 8 goroutines).
- On pgvector >= 0.8 the search enables `hnsw.iterative_scan = strict_order`, so a selective filter after an approximate search does not drop real matches.
- `Like` follows the chromem semantics (substring unless `*`/`%` wildcards; `_` literal).
- Password never logged; a test asserts it does not appear in connection errors.

What the tests prove: cosine scores equal the exact constructed cosines (within 1e-4), threshold direction and inclusivity, limit, filter + select-field projection in the same shape the semantic cache uses, every query operator, cursor pagination without duplicates or an empty trailing page, include-vectors round trip, DeleteAll results, namespace isolation, dimension-mismatch error (`ErrDimensionMismatch`), `ErrNotFound` text the cache matches on, rejection of NaN/Inf and wrong-dimension vectors, hostile namespace and field names, and that a cancelled context is an error, never an empty hit.

Not yet verified / still to do:
1. Whether `TestPgVectorStore_SelectiveFilterStillFindsMatch` exercised the HNSW + iterative-scan path or the GIN index (check with `EXPLAIN`); the test passes either way.
2. End-to-end through the semantic-cache plugin and the HTTP gateway (needs an embedding provider key; add pgvector to `plugins/semanticcache/test_utils.go`).
3. Registration touchpoints from section 6 (`transports/config.schema.json`, transports config, helm, terraform, docs page, `tests/docker-compose.yml` service; note the pinned-IP comments there).
4. `Benchmark*` with `-benchmem` and a cost statement, as `AGENTS.md` requires.
5. E1 evaluation harness, Tavily spike (needs key), organizer and maintainer outreach (user).

## 12. Hardening after the gate (2 Nov, same day)

### 12.1 Filtered search: a real false-miss bug that the store prevents
Probe in the pgvector container (16-dim vectors): 10,000 rows near the query under cache key A, 10,000 farther rows (cosine 0.80-0.86) under cache key B, filter `cache_key = B`, threshold 0.8, limit 1.

| Plan | Rows returned |
|---|---|
| PostgreSQL picks the HNSW index; `hnsw.iterative_scan` off (pgvector's default) | **0** (false miss: 10,000 rows qualify) |
| Same, `hnsw.iterative_scan = strict_order` (what the store sets on pgvector >= 0.8) | correct match, similarity 0.86 |
| Exact brute force (index scans disabled) | same match, 0.86 |

With a very selective filter (one matching row), PostgreSQL instead uses the GIN metadata index plus an exact sort, which is always correct. The regression test `TestPgVectorStore_BusyNeighbourKeyDoesNotHideMatches` reproduces the false miss without iterative scan (logged: 0 results) and asserts the store finds a match within 0.005 of the exact best. Failure mode without the fix is a cache miss (extra LLM call), never a wrong answer.

### 12.2 Binary vector encoding
Vectors were sent as text (`'[0.1,...]'::vector`). The store now registers pgvector's type on each pooled connection (`AfterConnect`, then `pool.Reset()` after the extension is ensured) and sends/reads vectors in pgvector's binary format; the text path remains as a tested fallback (`TestPgVectorStore_TextFallbackBehavesTheSame`).

| Encode one 1536-dim vector (i3-12100, Go 1.27.3) | ns/op | B/op | allocs/op | bytes on the wire |
|---|---|---|---|---|
| text literal (before) | 115,000-124,000 (63,000 when the machine was idle) | 55,296 | 3 | ~17 KB |
| binary (after) | ~1,300 | 0 | 0 | 6,148 |

### 12.3 Measurement caveat: Windows host -> Docker Desktop
From the Windows host, `Add` latency grows with message size: 8 ms (16 dims, 183 B), 12 ms (96), 14 ms (256), 60 ms (1536 dims, 17 KB text). The same 1536-dim insert inside the container takes ~7-9 ms (about 4 ms of that is `synchronous_commit=on` fsync on Docker's disk, ~2 ms HNSW maintenance). So host-run latency numbers mostly measure Docker Desktop's port proxy; representative numbers must be taken from a Linux client on the same Docker network (the evaluation agent was asked to do this).

### 12.4 Test status
30 tests pass (`go test ./vectorstore/ -run PgVector -count=1`), `go build` and `go vet` clean, gofmt clean on LF copies.

Local test database: container `bifrost-pgvector-test` on port 5433 (user `bifrost`, password `bifrost_password`, db `bifrost` - local test values only). Stop it with `docker stop bifrost-pgvector-test`.
