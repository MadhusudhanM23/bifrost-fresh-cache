# Bifrost vector-store evaluation harness

Evidence package for the PostgreSQL + pgvector vector-store backend added to
Bifrost (`framework/vectorstore/pgvector.go` in the local fork). Two tools:

- **`storebench` (Part 1)** measures the backend the way the semantic cache
  plugin uses it, side by side with the existing Qdrant backend, through the
  same `vectorstore.VectorStore` interface: write latency, direct-lookup
  latency, filtered nearest-neighbour latency, throughput, errors, and whether
  every nearest-neighbour answer is right (checked against an exact
  brute-force answer computed in Go).
- **`cachequality` (Part 2)** measures how well a similarity threshold
  separates prompts that should share a cached answer from prompts that should
  not, on a hand-written labelled dataset, both directly by cosine and through
  pgvector's `GetNearest`, and shows that the two agree.

Every number in this file was copied from a file the tools generated under
`results/`. Nothing here is estimated.

## Part 1 results (representative: in-Docker, N=10,000, K=20, median of 3 runs)

Full tables, every-run breakdown, environment, index state and query plans:
`results/storebench-in-docker-k20-interleaved-20261102-122207.md` (and `.json`).
Client ran inside an `alpine:3` container on a user-defined Docker bridge
network together with `bifrost-pgvector-test` and `eval-qdrant` — not the
Windows host. recall@1 was 1.000 for every GetNearest query in every run, for
both backends, at both dimensions; 0 errors throughout.

**D=1536**

| Operation | Conc | Backend | p50 (ms) | p95 (ms) | p99 (ms) | ops/s |
|---|---:|---|---:|---:|---:|---:|
| Add (write) | 1 | pgvector | 11.35 | 36.33 | 105.1 | 62 |
| Add (write) | 1 | qdrant | 3.87 | 6.67 | 10.75 | 242 |
| Add (write) | 8 | pgvector | 20.36 | 53.95 | 87.31 | 285 |
| Add (write) | 8 | qdrant | 24.06 | 52.29 | 72.68 | 294 |
| GetChunk (direct hit) | 1 | pgvector | 0.111 | 0.179 | 0.217 | 8796 |
| GetChunk (direct hit) | 1 | qdrant | 0.328 | 0.416 | 0.481 | 2942 |
| GetChunk (direct hit) | 8 | pgvector | 0.176 | 0.341 | 0.488 | 42066 |
| GetChunk (direct hit) | 8 | qdrant | 0.450 | 0.904 | 1.11 | 15549 |
| GetNearest (should hit) | 1 | pgvector | 3.74 | 5.36 | 8.94 | 252 |
| GetNearest (should hit) | 1 | qdrant | 0.936 | 1.41 | 1.77 | 988 |
| GetNearest (should hit) | 8 | pgvector | 5.78 | 12.41 | 16.21 | 1171 |
| GetNearest (should hit) | 8 | qdrant | 2.30 | 4.18 | 6.29 | 3218 |
| GetNearest (should miss) | 1 | pgvector | 3.44 | 4.71 | 5.64 | 285 |
| GetNearest (should miss) | 1 | qdrant | 0.748 | 1.00 | 1.14 | 1255 |
| GetNearest (cross-filter, should miss) | 1 | pgvector | 3.70 | 4.88 | 5.50 | 260 |
| GetNearest (cross-filter, should miss) | 1 | qdrant | 0.752 | 0.930 | 1.13 | 1301 |

**D=384**

| Operation | Conc | Backend | p50 (ms) | p95 (ms) | p99 (ms) | ops/s |
|---|---:|---|---:|---:|---:|---:|
| Add (write) | 1 | pgvector | 4.28 | 6.19 | 11.17 | 230 |
| Add (write) | 1 | qdrant | 2.90 | 4.92 | 8.18 | 289 |
| Add (write) | 8 | pgvector | 8.05 | 11.93 | 15.95 | 953 |
| Add (write) | 8 | qdrant | 19.24 | 40.81 | 94.60 | 338 |
| GetChunk (direct hit) | 1 | pgvector | 0.109 | 0.183 | 0.218 | 8471 |
| GetChunk (direct hit) | 1 | qdrant | 0.322 | 0.412 | 0.505 | 3046 |
| GetNearest (should hit) | 1 | pgvector | 3.13 | 5.17 | 5.73 | 287 |
| GetNearest (should hit) | 1 | qdrant | 0.631 | 0.763 | 0.880 | 1558 |
| GetNearest (should hit) | 8 | pgvector | 5.58 | 9.96 | 11.97 | 1276 |
| GetNearest (should hit) | 8 | qdrant | 1.27 | 1.93 | 2.34 | 6056 |

**Key findings, honestly stated:**
- GetChunk (the direct-id lookup that runs first on every cache request)
  favours pgvector: roughly 3x lower p50 than Qdrant at both concurrencies and
  dimensions.
- Add favours pgvector at D=384 (single-transaction insert, smaller payload);
  at D=1536 and at concurrency 8 the two are close, within run-to-run noise.
- GetNearest (the filtered approximate-NN lookup the semantic cache actually
  calls) favours Qdrant by roughly 3-4x at this N and K. `EXPLAIN ANALYZE`
  captured in the full results file shows why: with K=20 the metadata filter
  only narrows to ~500 rows, the planner estimates 1 matching row (no column
  statistics on the jsonb metadata at this width) and picks a Bitmap Heap Scan
  on the GIN metadata index plus an in-memory sort — pgvector's HNSW index on
  `embedding` is never used for this query shape. This is a real, reproducible
  finding about the current query plan at this N/K, not a defect in the
  GetNearest round-trip fix (which did measurably help: GetNearest here needs
  no separate `SET iterative_scan` statement, unlike before).
- Host-run numbers were also captured for context and are in the separate
  files below; they are NOT used as the representative numbers because the
  Windows-host-to-Docker network hop dominates latency at these payload sizes
  (see Caveats). They are consistent with the caveat: host p50s for pgvector
  writes were substantially higher than the in-Docker p50s above, while
  Qdrant's gRPC payloads were less affected.

Other in-Docker runs kept for the record (same workload, different K or
interleaving order, used to sanity-check that the K=20 result above is not an
artifact of phase ordering): `results/storebench-in-docker-k20-20261102-113532.md`,
`results/storebench-in-docker-k1-20261102-115021.md`.

## Layout

| Path | What |
|---|---|
| `cmd/storebench/` | Part 1 benchmark CLI (`main.go`) and its Markdown renderer |
| `cmd/cachequality/` | Part 2 threshold-sweep CLI |
| `internal/backend/` | Builds each store with `vectorstore.NewVectorStore`, plus a read-only side channel per server (versions, settings, index layout, query plans, wait-for-index). The side channel is never timed. |
| `internal/workload/` | Deterministic dataset, cache-shaped metadata, query classes, exact brute-force answers |
| `internal/stats/` | Closed-loop phase runner, nearest-rank percentiles, median of runs |
| `internal/clock/` | Latency clock: `QueryPerformanceCounter` on Windows, monotonic `time.Now` elsewhere |
| `internal/envinfo/` | Environment capture (CPU, OS, Go, Docker, source fingerprint) |
| `internal/embed/` | `Embedder` interface, OpenAI-compatible HTTP client, in-process hash embedder |
| `datasets/paraphrases.json` | 84 labelled prompt pairs (42 should hit, 42 hard negatives) |
| `scripts/build.ps1` | Builds `bin/storebench.exe` and `bin/storebench-linux`, stamped with a hash of the vectorstore sources |
| `scripts/run-in-docker.ps1` | Runs the Linux binary in an `alpine:3` container on a Docker network shared with the databases |
| `scripts/docker-network-cleanup.ps1` | Removes that network again |
| `results/` | Generated JSON (complete record) and Markdown (readable summary) of every run quoted here |

The module imports the local fork, not a release: `go.mod` has
`replace github.com/maximhq/bifrost/core => ../bifrost/core` and
`replace github.com/maximhq/bifrost/framework => ../bifrost/framework`. The fork
has no `go.work` and no `replace` directives of its own, and the framework only
requires `core` from the Bifrost repo, so those two lines are all it needs.

## What Part 1 measures, and why this way

The semantic cache plugin (`plugins/semanticcache`) calls the vector store in
four ways, and the benchmark times exactly those calls:

| Operation in the table | What the plugin does with it |
|---|---|
| Add (write) | stores every cached response (`utils.go`, `addNonStreamingResponse`) |
| GetChunk (direct hit / direct miss) | the direct cache lookup by deterministic id that runs first on every request (`search.go:38`); most requests miss |
| GetNearest (should hit / should miss) | the semantic lookup: `GetNearest(ns, vec, filters, selectFields, threshold, 1)` with Equal filters on `cache_key`, `params_hash`, `from_bifrost_semantic_cache_plugin=true` (`search.go:80-93`) |
| GetNearest (cross-filter, should miss) | the same call where a near-duplicate exists but under a *different* cache key: only the filter stands between the query and serving another tenant's answer |

Workload, identical for both backends at a given dimension (same seed):

- `CreateNamespace` gets the plugin's own `VectorStoreProperties` map (Qdrant
  turns every entry into a payload index; pgvector creates one GIN index over all
  metadata plus an HNSW index on the embedding).
- N entries (default 10,000) of random unit vectors, each with the metadata the
  plugin writes: `response` (a 1,024-byte chat-completion JSON), `stream_chunks`,
  `cache_key`, `params_hash`, `provider`, `model`,
  `from_bifrost_semantic_cache_plugin=true`, `expires_at`. Entry *i* belongs to
  cache key *i mod K* (default K=20, so each filter selects 500 entries). Ids are
  UUIDs (Qdrant requires them).
- Writes: 40% of N is preloaded at concurrency 8 (reported, not in the medians),
  then every run adds a fresh 10% of N at concurrency 1 and another at
  concurrency 8. The namespace therefore grows while writes are measured and
  holds exactly N entries when the query phases start.
- Before queries each backend is brought to its steady state: pgvector
  `VACUUM (ANALYZE)` (what autovacuum would do; without it the plan can change
  mid-run when autovacuum gets there), Qdrant waits until the collection is green
  with the optimizer idle.
- Query phases: 100 warm-up calls (not recorded), then 1,000 measured calls, at
  concurrency 1 and 8, repeated in 3 runs. The tables report the **median across
  runs** of each statistic; every run is in the JSON and in the "Every run" table
  of each results file.
- should-hit queries are unit vectors at a known cosine to a stored entry (drawn
  uniformly from 0.82 to 0.98 for threshold 0.8), filtered to that entry's cache
  key; should-miss queries are fresh random vectors; cross-filter queries are
  near-duplicates filtered to a different cache key.
- Correctness: every GetNearest answer is compared with the exact answer
  (float64 cosine over the query's filter partition, same threshold).
  recall@1 = correct top-1 / queries that have an exact answer; false hits =
  answers returned where the exact answer is empty. Returned entries are also
  checked for filter violations, scores below the threshold, and missing
  selected fields. GetChunk hits are checked for an intact `response` payload.
- Latency is per call, closed loop (each worker issues the next call as soon as
  the previous returns). Throughput = successful calls / wall time of the phase.
  Percentiles are nearest-rank over successful calls.
- Recorded with every result: client CPU/OS/Go, the latency clock and its
  resolution, Docker engine and image ids, PostgreSQL and pgvector versions and
  settings (`synchronous_commit`, `shared_buffers`, `hnsw.*`), the index
  definitions and sizes, sample `EXPLAIN ANALYZE` and generic plans of the exact
  GetNearest statement, Qdrant version, collection status, indexed-vector count,
  HNSW and optimizer config and payload indexes, the Bifrost git head and a hash
  of the vectorstore sources stamped into the binary at build time.

Why the latency clock matters on Windows: Go's `time.Now` here advances in
steps of about 0.3 to 0.6 ms (measured: a 50 µs busy loop read as 1 ms), too
coarse for sub-millisecond calls, so on Windows the harness reads
`QueryPerformanceCounter` (100 ns steps, measured). Inside the Linux container
`time.Now` is fine (32 ns median step, recorded in the results).

## Running Part 1

Prerequisites: Go 1.27.3 (here at `C:\Program Files\Go\bin`, not on PATH in new
shells), Docker Desktop, the pgvector container (`bifrost-pgvector-test`,
`pgvector/pgvector:pg16`, localhost:5433, user `bifrost`, password
`bifrost_password`, database `bifrost`: local test values).

```powershell
cd C:\Madhusudhan\Geeks\hackathon-workspace\eval-harness
$env:Path = "C:\Program Files\Go\bin;" + $env:Path

# Comparison backend (same version as the repo's test suite)
docker run -d --name eval-qdrant -p 6334:6334 qdrant/qdrant:v1.16.0

# Build both binaries from the same source state, stamped with a source hash
powershell -ExecutionPolicy Bypass -File scripts\build.ps1

# Representative run: client inside Docker, next to the databases
powershell -ExecutionPolicy Bypass -File scripts\run-in-docker.ps1 -Label in-docker-k20 -BenchArgs "-dims 1536,384 -n 10000 -keys 20 -queries 1000 -warmup 100 -runs 3"

# Same benchmark from the Windows host (through Docker Desktop's port proxy)
bin\storebench.exe -label windows-host-k20 -dims 1536,384 -n 10000 -keys 20 -queries 1000 -warmup 100 -runs 3

# Re-render a results file's Markdown from its JSON
go run ./cmd/storebench -render results\<file>.json

# Clean up: namespaces (the runs drop their own; this catches interrupted runs), network, Qdrant
bin\storebench.exe -cleanup-only -prefix eval_
powershell -ExecutionPolicy Bypass -File scripts\docker-network-cleanup.ps1
docker rm -f eval-qdrant
```

Useful flags: `-backends pgvector,qdrant[,redis]`, `-dims`, `-n`, `-keys`,
`-queries`, `-warmup`, `-runs`, `-concurrency 1,8`, `-threshold 0.8`,
`-response-bytes 1024`, `-seed 42`, `-keep` (leave namespaces in place),
`-pg-host/-pg-port/-pg-user/-pg-db` (password from `PGVECTOR_PASSWORD` or
`-pg-password`), `-qdrant-host/-qdrant-port`, `-redis-addr`.
Namespaces are `<prefix>_<backend>_d<dim>` (default prefix `eval_bench`);
a leftover namespace with the same name is dropped before a run starts.

## Running Part 2 with a real embedding model

One command, with the key in an environment variable (it is never taken on the
command line or written to any output):

```powershell
$env:OPENAI_API_KEY = "<your key>"; go run ./cmd/cachequality -label openai-3-small
```

Defaults: `-base-url https://api.openai.com/v1 -model text-embedding-3-small
-thresholds 0.70:0.98:0.02 -store pgvector` (localhost:5433 as above). Any
OpenAI-compatible endpoint works: set `-base-url`, `-model`, and
`-api-key-env <name of the variable holding its key>`; `-dimensions N` sends the
optional `dimensions` field. `-format chat` (default) embeds
`user: <lowercased prompt>`, which is what the semantic cache embeds for a
one-message chat request (`extractTextForEmbedding` + `normalizeText`);
`-format raw` embeds the prompt as written. `-store none` skips the pgvector
cross-check. Output: `results/cachequality-<label>-<time>.json` and `.md`.

A run is labelled *not meaningful for quality* automatically when the endpoint
is the local mock server (it answers `GET /mock/stats`), when the model name
contains "mock", or with `-embedder hash`.

## Caveats (read before quoting any number)

- **Docker Desktop on Windows.** From the Windows host every call crosses
  Docker Desktop's userspace port proxy, which adds latency that grows with
  message size. pgvector receives each vector as text (about 17 KB at 1536
  dimensions) while Qdrant receives about 6 KB of binary gRPC, so host-run numbers
  are skewed against pgvector. The representative numbers are the in-Docker runs
  (client in a container on the same Docker network as the databases); the
  host-run numbers are shown separately, labelled, to make the effect visible.
- **One shared, busy machine.** Client, PostgreSQL and Qdrant share one
  4-core / 8-thread i3-12100 (the Docker VM sees 8 CPUs and 7.65 GiB) with no
  resource limits, and the machine was in use by other work at the same time
  (IDE, browser, other agents' builds and tests). Host CPU, sampled every 5 s,
  reached 95 to 100% during parts of the runs. That is why the run-to-run
  ranges (shown in every table) are wide. WSL2 reports no steal time, so the
  harness cannot see outside load from inside the VM; it records the VM's own
  CPU use per phase instead. Treat differences smaller than the run-to-run range
  as noise.
- **Local, single node, small.** 10,000 entries per namespace, one PostgreSQL
  and one Qdrant node with default settings (`shared_buffers=128MB`,
  `synchronous_commit=on` as found; nothing was tuned). Production numbers on
  dedicated hardware will differ; the comparison between the two backends on the
  same box is the useful part.
- **Random vectors, not real embeddings.** Random unit vectors are almost
  orthogonal to each other, which is the easiest case for an approximate index:
  there are no dense neighbourhoods. recall@1 = 1.000 here shows that filters,
  threshold and scores are handled correctly; it does not show recall on real
  embeddings, which cluster. (At K=20 neither backend's approximate index is
  even on the query path; see the findings.)
- **pgvector's plan depends on statistics.** The plans recorded in the results
  are for this data. The finding that GetNearest never uses the HNSW index rests
  on the metadata column having no planner statistics, which holds whenever the
  stored response makes the metadata wider than 1 KB.
- **Writes commit one by one.** Each `Add` is its own transaction
  (pgvector) or a `wait=true` upsert (Qdrant), as the stores implement them;
  write latency includes an fsync on Docker's virtual disk.
- **Part 2 mock numbers are not quality numbers.** See Part 2.
- **Code under active development.** The fork was being edited by others during
  this work. Every results file records the git head and the hash of the
  vectorstore sources compiled into the binary (and whether it still matched
  the files on disk at run time).

## Impact benchmark (`impactbench`): Bifrost alone versus Bifrost with the freshness companion

`cmd/impactbench` replays one scripted, seeded workload against the same running
Docker stack through different doors and counts what each door serves. The
right answer to every request is what a **fresh, uncached call to the mock
model** returns for that exact request. Every number in this section was copied
from `results/impactbench-20261105-123632.md` (and its `.json`, which also holds
every request of every run).

**The story.** A support bot whose app puts the *current* price into the system
message at request time; the demo page is the source of truth.
(1) Warm the cache at $49 (price questions in five wordings, three look-alike
pairs, two ordinary questions). (2) Change the page to $59, wait 6 s.
(3) Ask again: the same and new price wordings, the look-alike pairs in the
opposite order, same-meaning rewordings, and four requests that carry a role
`tool` message. (4) Change the page to $69, wait 6 s, fire 16 identical price
requests at the same moment at the now-stale entry. 56 requests per arm per run.

**Arms** (own cache key each, fresh for every run; arm order rotated each run):
`plain` = straight to Bifrost (semantic cache, threshold 0.8, TTL 30m,
`exclude_system_prompt=true`, so a new price in the system message still matches an
entry written under the old one); `companion` = the freshness companion in front
of that same Bifrost (gate `-verify-below=0.90`, audit of every semantic hit,
guard on, evidence TTL 5 s); `plain-ttl` = straight to Bifrost with
`x-bf-cache-ttl: 5s`, included so the plain arm is not a strawman: it is the
"just expire entries quickly" alternative.

**Headline (median of 3 runs; mock model, mock embeddings, 56 requests per arm).**

| Metric | plain | companion | plain-ttl |
|---|---:|---:|---:|
| Served from the cache (hit rate) | 44 (78.6%) | 21 (37.5%) | 19 (33.9%) |
| Model calls, all causes | 12 | 32 | 37 |
| ...answering requests (misses, refreshes) | 12 | 19 | 37 |
| ...pre-serve gate checks | 0 | 7 | 0 |
| ...background audits | 0 | 6 | 0 |
| Stale answers served (old price) | 28 | 0 | 0 |
| Wrong-match answers served | 4 | 2 | 5 |
| Errors | 0 | 0 | 0 |
| **Bad answers per 1000 requests** | **571.4** | **35.7** | **89.3** |
| Tool-result requests answered from the cache (of 4) | 2 | 0 | 2 |
| Burst of 16: stale answers / errors / model calls | 16 / 0 / 0 | 0 / 0 / 1 | 0 / 0 / 16 |
| Latency p50 / p95, sequential requests (ms) | 3.6 / 106.0 | 106.1 / 114.6 | 104.2 / 106.8 |
| ASSUMED cost per 1000 requests (model calls x $0.002) | $0.429 | $1.143 | $1.321 |

The cost row multiplies measured call counts by an **assumed** $0.002 per model
call (`-assumed-usd-per-call`); it is an illustration, not a measurement.

**What the numbers say, and what they do not.**

- The companion removed every stale price answer (28 in the plain arm, 16 of them
  in the burst) and fixed the password/username look-alike (the gate compared the
  cached answer with a fresh one and replaced the wrong one). It also kept all four
  tool-result requests out of the cache; plain Bifrost answered two of them from the
  cache although the tool data had changed.
- **It did not fix the two-factor on/off pair.** The mock embedding scores that pair
  at cosine 1.0 ("on" and "off" are stopwords), above the gate's 0.90, so the wrong
  answer was served first. The audit flagged it afterwards (2 audits found a
  difference per run), after the user had received it. No similarity threshold can
  separate a pair that scores 1.0. This pair is the companion's 2 remaining bad answers
  per run; the plain arm has the same two plus the password/username pair.
- **It costs model calls.** 32 versus 12 in the plain arm: 7 gate checks, 6 audits
  and 7 more answering calls (each stale entry that is refreshed needs a real
  model call, where plain served the stale answer for free). Against calling the
  model for all 56 requests, plain saved 44 calls and the companion 24. Part of
  plain's saving is the stale answers.
- **It costs latency where it matters, by design.** Sequential p50 is 106.1 ms
  against 3.6 ms because the companion spent a model call on the requests plain
  answered with a stale entry in a few milliseconds. The mock model answers
  instantly and still takes about 105 ms through this stack, so on a real model the
  refresh cost is one real model call, not a proxy hop. Cache hits that need no check
  stay fast (p50 4.1 ms versus 3.1 ms for plain).
- **The burst.** Sixteen concurrent identical asks at a stale entry: plain served
  16 stale answers, the companion made 1 model call for all 16, the
  short-TTL arm made 16 (every request missed at once).
- **A short TTL is not free.** `plain-ttl` also removed the stale answers, but its hit
  rate fell to 33.9%, it made 37 model calls (about three times the plain arm) and
  it had 5 wrong matches: with entries expiring, a look-alike neighbour can outlive
  the entry that would have been right. Its figures depend on timing between
  requests (the TTL is 5 s); read them as the shape of the trade-off.
- The CSV export/import pair scores 0.7184 with the mock embedding, below the 0.8
  threshold, so it never confused anything here. It stays in the workload as written
  (not re-worded until it collides).

**Limits.** Mock model and mock embeddings (look-alikes match because of shared
words, not meaning); a small scripted workload, so per-1000 rates are this story
scaled up, not a forecast; run-to-run variation here only comes from the order of
independent items and timing, so the three runs show noise and order effects, not
statistical confidence (the companion's model calls were 32, 35 and 32 in the three
runs, plain's 12 each time). The model has no artificial latency. Evidence fetches are
not counted as model calls. The page and the stack must not be used by anything
else during a run (the command checks that the mock's counters are idle at the
start). The full list is at the bottom of the results file.

**Reproduce** (stack up with `docker compose up`, nothing else using it or the page
`freshness-companion/demo-page/index.html`, which the command edits and restores
byte for byte, also on error or Ctrl-C). From `eval-harness/`, with Go on the PATH
(`$env:Path = "C:\Program Files\Go\bin;" + $env:Path`):

```powershell
go run ./cmd/impactbench -repeat 3 -arms plain,companion,plain-ttl
```

About 5 minutes. Output: `results/impactbench-<time>.json` and `.md`. Useful flags:
`-arms plain,companion` (the two-arm version), `-seed`, `-burst`, `-settle`
(must exceed the companion's evidence TTL), `-assumed-usd-per-call`,
`-assumed-usd-per-embedding`, `-dry-run` (print the workload for a seed, touches
nothing), `-render results/<file>.json` (rebuild the Markdown from a results file).
Unit tests (`go test ./cmd/impactbench`) use in-process fakes and need no stack.

## Hybrid search spike: is "keywords + meaning" worth it?

`cmd/hybridspike` asks whether look-alike questions ("export CSV" vs "import CSV",
"two-factor on" vs "off") are separated better when a keyword check is added to the
vector similarity. It uses the same 84 labelled pairs as `cachequality`
(`datasets/paraphrases.json`: 42 should-share, 42 should-not-share) and the same
embedding text, loads them into its own schema `hybridspike` on the scratch
PostgreSQL (port 5433), lets PostgreSQL compute the cosine (pgvector) and the
full-text facts (`to_tsvector`, `plainto_tsquery`, `ts_rank`), and drops the schema.

```powershell
go run ./cmd/hybridspike -embedder hash                       # in-process, no server
$env:MOCK_OPENAI_KEY = "sk-mock-local"                        # the local mock's fake key
go run ./cmd/hybridspike -embedder openai -model mock-embedding -label mock-openai
```

Results: `results/hybridspike-<label>-<time>.md` and `.json`. **Both embedders are
bag-of-words, so the numbers flatter a keyword rule** (the vector alone even scores
look-alikes higher than paraphrases here). Read the report's "Read this first" list
before quoting anything; a real embedding model is needed for a real answer.
Unit tests: `go test ./cmd/hybridspike` (pure parts, no database).
