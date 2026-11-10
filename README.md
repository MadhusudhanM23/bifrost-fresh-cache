# Bifrost Fresh Cache

**A Postgres/pgvector backend for Bifrost's semantic cache, and a small companion that stops the cache from lying to you.**

Built for the Geek Room Global AI Hackathon 2026, Track A, Problem 04: *build the missing open-source gateway feature*. It uses [Bifrost](https://github.com/maximhq/bifrost) as the gateway we extend, and [Tavily](https://tavily.com) as the eyes that check whether a cached answer is still true.

![Architecture: app, freshness companion, Bifrost with the new pgvector store, PostgreSQL, Tavily, model, dashboard](docs/images/architecture.svg)

---

## The itch we scratched

A semantic cache is a great idea with a quiet failure mode. It remembers answers and hands one back whenever a new question *looks like* an old one. That saves model calls and latency. It also keeps saying "$49" long after the pricing page moved to $59, confuses "reset my username" with "reset my password", and happily reports a rising hit rate while it does so. Every one of those replies comes back `200 OK`. Nobody notices until a customer does.

We hit two versions of this problem while reading Bifrost's code and issue tracker:

1. **You couldn't put the cache in Postgres.** Bifrost's semantic cache could talk to Chromem, Pinecone, Qdrant, Redis and Weaviate, but not PostgreSQL, even though pgvector is sitting in most teams' databases already. Running one more database just for a cache is a real operator cost. ([maximhq/bifrost#3180](https://github.com/maximhq/bifrost/issues/3180), open since May 2026.)
2. **Nothing checked whether a cached answer was still right.** A TTL is a guess. The source changes when it changes.

So we built both halves.

## What's in the box

| Part | What it does | Where |
|---|---|---|
| **pgvector VectorStore** for Bifrost | A full `VectorStore` implementation (all 13 methods), config and Helm wiring, docs, and 32 tests that run against a real PostgreSQL. Written to be a PR, not a hack. | [`bifrost/`](bifrost) fork, branch `feat/pgvector-vectorstore`, mainly `framework/vectorstore/pgvector.go` |
| **Freshness companion** | A reverse proxy in front of Bifrost. Before it reuses a time-sensitive answer, it reads the live source (through Tavily Extract) and compares. It also gates risky look-alike hits, keeps tool results and secrets out of the cache, shares one refresh across a stampede, audits a sample of reuses, and explains every decision. With `-mcp` it also offers these checks as four MCP tools, which Bifrost's MCP gateway can expose. Go, standard library only. | [`freshness-companion/`](freshness-companion) |
| **Postgres-only extras** | Row-level security as a second tenant wall enforced by the database, cleanup of expired rows (they otherwise live forever), `halfvec` compact vectors (about half the disk in our test), and plain SQL views over the cache. | `bifrost/` docs, [`sql/`](sql) |
| **New: receipts, smart checking, watcher, MCP** | Every checked answer now carries a one-line **receipt** (what was checked, when, what the page said). Opt-in: **smart checking** re-checks pages that never change less often (92% fewer page reads on a quiet page in our simulation), a **background watcher** re-checks busy pages ahead of time and deletes answers that state an old value, and **MCP mode** lets any agent call the checks as tools through Bifrost's MCP gateway. | [Answer receipts](#answer-receipts-every-checked-answer-says-what-was-checked), [Checking less often](#checking-less-often-and-checking-before-you-ask), [DOCKER.md](DOCKER.md#mcp-mode) |
| **Dashboard** | Answers "is the cache saving money, and is it telling the truth?" in plain words, with a Try-it panel that runs the same scenario through a plain cache and through ours, side by side. | [`dashboard/`](dashboard) |
| **Evidence** | Store benchmarks against Qdrant, a threshold-quality harness, an impact benchmark, and a 16-step end-to-end run through the real gateway. | [`eval-harness/`](eval-harness), [`e2e/`](e2e) |

## See it fail, then see it not fail

The dashboard runs the same question through two caches. Left: Bifrost's cache on its own. Right: the same cache with the companion in front. We changed the price on the demo page between the two asks.

![Dashboard: plain cache hands back the stale $49 answer; with the companion the old answer is dropped and the model is asked again, giving $59](docs/images/dashboard-price-changed.jpg)

And the summary after running all four scenarios (a price change, two look-alike questions, an answer built on live tool data, and a burst of ten people asking at once):

![Dashboard overview: 14 wrong or out-of-date answers stopped, 12 model calls saved](docs/images/dashboard-overview.jpg)

The yellow banner is deliberate. Those numbers come from a mock model and mock embeddings, so they show how the safety net behaves, not how a real model would score.

## Measured impact

Same scripted workload, same Bifrost, three setups. `plain` is Bifrost with its defaults (threshold 0.8, 30-minute TTL). `companion` adds our proxy. `plain-ttl` is the usual workaround, a 5-second TTL. Median of 3 runs, 56 requests per setup per run. Full report: [`eval-harness/results/impactbench-20261105-123632.md`](eval-harness/results/impactbench-20261105-123632.md).

| | plain | **companion** | plain-ttl |
|---|---:|---:|---:|
| Stale answers served (old price) | 28 | **0** | 0 |
| Wrong-match answers served | 4 | **2** | 5 |
| **Bad answers per 1,000 requests** | 571 | **36** | 89 |
| 16 simultaneous asks at a stale entry: stale answers / model calls | 16 / 0 | **0 / 1** | 0 / 16 |
| Answers reused although the request carried live tool data (of 4) | 2 | **0** | 2 |
| Cache hit rate | 78.6% | 37.5% | 33.9% |
| Model calls | 12 | 32 | 37 |
| Latency p50 | 3.6 ms | 106 ms | 104 ms |

The honest reading: the companion removes every stale answer and most wrong matches, and it survives a burst with one model call instead of sixteen. It is not free. It makes more model calls and answers more slowly than a plain cache that is happy to be wrong, because every refresh is a real call the plain cache skipped. Against the short-TTL workaround it wins on all three counts: fewer bad answers, a slightly higher hit rate, fewer model calls. The two wrong matches it still lets through are a "two-factor on / two-factor off" pair that the mock embedding scores at similarity 1.0, which no threshold can separate; the background audit flags them afterwards.

### With a real model

The same workload was run once (not three times) with answers from a real model, `groq/openai/gpt-oss-20b` on Groq's free tier. Embeddings stayed on the mock, so the cache's idea of "similar" is unchanged. Full report, with every setting that differs from the mock run and why: [`eval-harness/results/impactbench-real-groq-20261110-173322.md`](eval-harness/results/impactbench-real-groq-20261110-173322.md). 91 model requests in total.

| | plain | **companion** | plain-ttl (30 s) |
|---|---:|---:|---:|
| Stale answers served (old price) | 28 | **0** | 0 |
| Wrong-match answers served | 4 | **2** | 5 |
| Errors (HTTP 429 from the free tier) | 0 | **0** | 8 |
| **Bad answers per 1,000 requests** | 571 | **36** | 232 |
| 16 simultaneous asks at a stale entry: stale answers / model calls | 16 / 0 | **0 / 5** | 0 / 8 (+8 refused) |
| Answers reused although the request carried live tool data (of 4) | 2 | **0** | 2 |
| Cache hit rate | 78.6% | 37.5% | 28.6% |
| Model calls | 12 | 37 | 32 |
| Latency p50 | 4.4 ms | 471 ms | 475 ms |

What the real model changed: the stale and wrong-match counts came out the same as with the mock (the cache behaves the same because the embedding is the same). The companion cost more here, 37 calls instead of 32, because its gate now uses a second model call as a judge (real answers never match word for word, so exact comparison was turned off) and because the burst took 5 calls rather than 1 (with a real call lasting 0.5-1 s, not every concurrent request joined the shared refresh). Background audits were turned off to save calls, so the two-factor wrong match was not flagged afterwards this time. Non-cached answers now take about half a second, which is the model, not the companion; cache hits still take about 4 ms in every setup. The short-TTL arm's 8 errors are the free tier refusing 16 simultaneous misses (tokens-per-minute limit); on a paid tier it would have had 5 bad answers, not 13. One run, so read these as what happened once.

## Run it

You need Docker Desktop (or Docker Engine with Compose v2) and Git. No API keys. The default stack uses a local stand-in model so anyone can run it.

```bash
git clone --recurse-submodules https://github.com/MadhusudhanM23/bifrost-fresh-cache.git
cd bifrost-fresh-cache
docker compose up --build --wait
```

The first build takes a few minutes (it compiles Bifrost from the fork). Then:

| What | Where |
|---|---|
| Freshness companion (send your requests here) | http://127.0.0.1:8090 |
| Bifrost gateway, stock, with the pgvector store | http://127.0.0.1:18090 |
| Demo pricing page the companion watches | http://127.0.0.1:8091 |
| PostgreSQL 16 + pgvector 0.8 | `127.0.0.1:5434`, user `bifrost`, password `bifrost_password` |

Ask the price question twice and watch the headers go from a miss to a confirmed hit (`x-bifrost-cache-hit`, `x-freshness-status`):

```bash
curl -s -D - http://127.0.0.1:8090/v1/chat/completions \
  -H "Content-Type: application/json" -H "x-bf-cache-key: demo" \
  --data-binary "@docker/request-49.json"
```

Now open `freshness-companion/demo-page/index.html`, change `$49` to `$59`, and send the same request a third time. The companion reads the page, sees the cached answer is out of date, drops it and asks the model again: `x-freshness-status: stale`. [DOCKER.md](DOCKER.md) walks through this and the other demos step by step, including the look-alike, tool-data and stampede cases. (On Windows use `curl.exe`.)

**Dashboard** (needs Go):

```bash
cd dashboard
go run . -demo-page ../freshness-companion/demo-page/index.html
# open http://127.0.0.1:8095
```

**Smoke test** of the running stack (18 checks): `powershell -File smoke.ps1`. **SQL views** test (45 checks): `powershell -File sql/test.ps1`. Bash versions of the SQL scripts are in `sql/` too.

### With a real model

Put a key in `.env` (copy `.env.example`; Groq, OpenRouter and xAI are wired) and start with the overlay:

```bash
docker compose -f docker-compose.yml -f docker-compose.real.yml up -d
powershell -File real-model-check.ps1
```

Embeddings still come from the local mock in this mode. See the "Real model" section of [DOCKER.md](DOCKER.md).

### With Tavily

Put `TAVILY_API_KEY` in `.env` and start with the Tavily overlay:

```bash
docker compose -f docker-compose.yml -f docker-compose.tavily.yml up -d
```

The companion now reads the watched price through Tavily Extract from the public copy of the demo page, https://madhusudhanm23.github.io/bifrost-fresh-cache/acme-pricing/ (source in `docs/acme-pricing/`, served by GitHub Pages). Tavily cannot reach a page that exists only on your machine, which is why the default stack reads the local page directly instead.

Proven live on 10 Nov 2026: the price question was cached while the page said $49 (Tavily: `confirmed`), the page was changed to $59, and the next ask came back `x-freshness-status: stale`, `x-freshness-action: corrected`, with the answer "$59 per month" and the source URL. One thing we learned on the way: Tavily keeps a remembered copy of a URL it has seen, so the companion stamps each request with a unique `_fresh=` parameter (`-tavily-fresh`, on by default).

## How the two technologies are used

**Bifrost** is the thing we extend, not just a thing we call. The pgvector store plugs into Bifrost's `VectorStore` interface, registers through its config schema and Helm chart, and is covered by Bifrost's own shared contract tests plus our own. The companion sits in front of a *stock* Bifrost build and drives it through its public API: `x-bf-cache-key` for tenant buckets, `x-bf-cache-no-store` to keep things out, `DELETE /api/cache/clear/{id}` to evict a stale entry. Nothing in the companion depends on a private hook. The connection also runs the other way: in MCP mode (`docker-compose.mcp.yml`) the companion serves a small MCP server and Bifrost registers it as an MCP client, so Bifrost's MCP gateway exposes `freshness-check_freshness`, `freshness-explain_request`, `freshness-list_watched_sources` and `freshness-report_change` to any agent or client that uses it. Off by default; see [DOCKER.md](DOCKER.md#mcp-mode).

**Tavily** is the companion's evidence. When a cached answer is about a watched fact (a price, a policy, an opening time), the companion asks Tavily Extract for the current page, pulls the value with a configured pattern, and compares it with the value in the cached answer. With discovery off (the default), only configured source URLs go to Tavily. With `-discover`, Tavily Search also receives a short query built from the policy's template and product names found in the question, never the question or the answer. Prompts, answers and credentials never go to Tavily. If Tavily is unreachable the response says so in a header (`unable_to_verify`) instead of pretending.

## How the pgvector backend works

```
app  →  Bifrost  →  semantic-cache plugin  →  VectorStore interface  →  pgvector store  →  PostgreSQL + pgvector
```

- **One table per namespace**: `id TEXT PRIMARY KEY, embedding vector(dim) NULL, metadata JSONB`. A table comment marks what the store owns; it never adopts or drops a table it did not create.
- **Cosine exactly as Bifrost defines it.** pgvector's `<=>` returns distance, so we report `1 - distance` and apply the threshold as `distance <= 1 - threshold`. Tests check scores against vectors built at known cosines.
- **Indexes**: GIN on the metadata for the cache's equality filters; HNSW (`vector_cosine_ops`) for similarity up to pgvector's 2,000-dimension limit, exact search with a warning above it.
- **No false misses on filtered search.** With 10,000 entries under *another* cache key sitting nearer the query, a default HNSW scan returned 0 rows although 10,000 qualified. We turn on `hnsw.iterative_scan` (pgvector 0.8+) once per connection and have a regression test for it.
- **Binary vectors over pgx**: about 6 KB instead of 17 KB on the wire for a 1,536-dimension vector, and encoding drops from roughly 115 µs with 55 KB of allocations to about 1.3 µs with none. The text path stays as a tested fallback.
- **Safety**: every filter value and field name is a bind parameter; namespace and schema names are validated and quoted; start-up across replicas is serialised with an advisory lock; a database error is never turned into a cache hit; the password never appears in logs or errors (tested).
- **Metadata-only rows**: the embedding column is nullable, so direct-hash entries need no placeholder vector.

32 pgvector tests plus Bifrost's shared contract tests pass against PostgreSQL 16.15 / pgvector 0.8.7 (`go test ./vectorstore/ -run PgVector -count=1` in `bifrost/framework`). `go build`, `go vet` and `gofmt` are clean.

## Answer receipts: every checked answer says what was checked

Every answer that went through a freshness check carries one extra header, `x-freshness-receipt`, that sums the check up in a single line: the verdict, what the source page said, how many seconds before this answer it was read, where and how it was read, and whether that reading was fresh or reused notes.

```
x-freshness-receipt: v=1; verdict=confirmed; action=served; found="59"; checked_ago_s=12; source="http://demoserver:8000/"; provider="direct_fetch (not Tavily)"; checked_at="2026-11-10T09:00:00Z"; evidence=reused; stated="59"; fact="Acme Pro plan price"; answer=cached
```

Think of it as the till receipt stapled to the parcel: it does not change what is inside, and you can check the sum. The answer body is never touched, so receipts are always on (no flag): they only add the `x-freshness-receipt` and `x-freshness-evidence` headers. `demo-ask.ps1` prints the receipt in words, `GET /freshness/explain/{id}` returns it as a `receipt` object, and the dashboard shows it under each Try-it step on the companion's side and in the request list. Verified answers and streamed answers have no receipt, because they have no freshness status. Format and keys: [freshness-companion/README.md](freshness-companion/README.md#answer-receipts).

## What the companion checks, in order

1. **Guard.** Requests that carry a tool result, offer an action tool, or contain an email, card number, token or similar never touch the cache.
2. **Verified answers.** A small human-approved table, matched on exact wording, served first and still checked against the live source when the topic is watched.
3. **Gate.** A semantic hit in the risky band just above the threshold (below 0.90 in the demo) is re-asked *before* it is served; a wrong one is replaced and the right answer cached for that exact question.
4. **Freshness.** For a time-sensitive hit, read the live source, compare, and on a mismatch clear and re-ask. Outcomes: `confirmed`, `stale`, `unable_to_verify`, `unknown`.
   - *Opt-in discovery (`-discover`):* when no watched fact covers the hit, Tavily Search looks for a source page with a short query from the policy, and the answer is checked against that page the same way. If it cannot settle the answer, the companion does exactly what it would have done without discovery.
5. **Wrong from the start.** A fresh answer whose number disagrees with the verified source is reported, removed or re-asked.
6. **Change notices.** `POST /freshness/changes` marks entries for re-check on their next hit.
7. **Stampede.** Everyone waiting on the same refresh shares one model call.
8. **Audit.** A sample of reuses is re-asked afterwards and turned into a wrong-match rate and a suggested threshold.
9. **Explain.** `GET /freshness/explain/{id}` says in plain words how a request was answered, without the prompt or answer text, and every checked answer carries a receipt header.

Opt-in extras (off by default): **smart checking** re-checks quiet pages less often, the **background watcher** re-checks active pages ahead of time and deletes answers that state an old value, **MCP mode** offers the checks as tools through Bifrost's MCP gateway, and **discovery** lets Tavily Search find the source page when no watched fact covers a question. See the sections above and below.

Every response carries `x-freshness-*` headers with the outcome. Details, flags and the test list are in [freshness-companion/README.md](freshness-companion/README.md). `go test ./...` there runs 319 tests counting subtests (10 Nov 2026).

## Checking less often, and checking before you ask

Two opt-in features, both **off by default** so every demo above is unchanged:

- **Smart checking** (`CHECK_ADAPTIVE=true`): a page that keeps saying the same thing is re-checked less and less often, from the evidence TTL up to a cap (`CHECK_MAX`, 60 s in docker, 5 min as the flag default); the moment it changes, the checks go back to the fastest rate. Measured on a virtual clock with the real code ([`eval-harness/results/checksim-20261110.md`](eval-harness/results/checksim-20261110.md)): on a page that changes once a day, 92% fewer evidence fetches than a fixed 5 s check, at the cost of a handful of stale answers around the change (up to the cap); on a page that changes every 10 minutes with a 5-minute cap, a quarter of the answers were stale, so such a page needs a short cap or change notices. What "confirmed" means does not change; how old the evidence behind it may be does, and the header `x-freshness-checked-at` says exactly when it was checked.
- **Background watcher** (`WATCH=true`): re-checks pages people are actually asking about shortly before their evidence expires, so requests find fresh evidence and do not wait, and deletes the cached answers it knows state the old value once it sees a change. Capped per hour (`WATCH_MAX_PER_HOUR`). Two honest caveats from the same report: at low traffic it can fetch more than on-demand checking, and combined with smart checking it served **more** stale answers in the simulation than smart checking alone (quiet trace 11 vs 5, busy-sale trace 263 vs 180), because keeping evidence warm also keeps old evidence in use until the next scheduled look. It only reached zero stale answers when the page owner also sent change notices (`POST /freshness/changes`). Use it with `CHECK_ADAPTIVE=true`: on its own it re-checks every few seconds and uses up its hourly cap in minutes.

`GET /freshness/watch` shows every source's interval, last values and next check. Details in [freshness-companion/README.md](freshness-companion/README.md#smart-checking-and-the-background-watcher), the docker opt-in in [DOCKER.md](DOCKER.md#opt-in-smart-checking-and-the-background-watcher).

## Tavily finds the source (opt-in)

A watched fact needs someone to write down the page URL and a pattern for every price. Discovery (`-discover`, **off by default**) covers the questions nobody wrote a fact for. For a time-sensitive cache hit with no watched fact, in a category whose policy has a `discover` block:

1. The companion builds a short query from the policy's template and the product names it finds in the question (only names on the policy's allowlist), for example `Acme Pro plan price per month`. A query that looks sensitive (email, card, token...) or is longer than 80 characters is not sent.
2. One Tavily Search call (1 credit, capped per hour) returns a few results. The companion keeps only https pages on the allowed domains, not on forums or marketplaces, with a good enough score and the product name in the title or snippet, and prefers the product's own site.
3. Tavily Extract reads that page (as for a watched fact) and the companion looks for exactly one price right after the product name. The cached answer must state exactly one price too. Different: `stale`, and the answer is refreshed. Same price: `confirmed`, but that serves the answer only when the policy limits search to named sites (`include_domains`); for a page from the open web the companion does what it would have done without discovery, so an open-web page can trigger a refresh but never lets an old answer through. Anything unclear (a pricing table with several prices, no price, a fetch error): the companion does exactly what it would have done without discovery.
4. The page is remembered for 24 hours (and "found nothing" for an hour), so a popular question costs one search, not one per ask.

Think of it as asking a librarian for the right book, once, and then reading the price yourself every time. A page found this way may trigger a refresh, but its text never goes into a prompt: `on_contradicted: correct` is treated as `uncache` for it. Every such answer says so: `x-freshness-discovery`, `x-freshness-discovery-query` (the exact text sent to Tavily), `x-freshness-found-by`, and `discovered=true; found_by=...; query="..."` in the receipt. `GET /freshness/discovered` lists what was learned.

Live check (10 Nov 2026, 5 Tavily credits counted conservatively, [`eval-harness/results/discover-live-20261110.md`](eval-harness/results/discover-live-20261110.md)): Tavily Search did **not** find the GitHub Pages demo page (0 results for the allowed domain), so the policy's fallback `candidate_urls` entry was read instead and labelled `found_by=candidate_url`; with it, $49 was confirmed and a $59 answer was caught as stale and its entry deleted, with zero extra credits on repeat asks. A real-world question (Raspberry Pi 5, raspberrypi.com) found a news page, Tavily could not extract it, and the answer was left unverified rather than confirmed. Discovery is a convenience with a safety net, not a guarantee of finding the right page. Setup: [DOCKER.md](DOCKER.md#opt-in-tavily-finds-the-source).

## Proof it works end to end

[`e2e/`](e2e) builds `bifrost-http` from the fork, points it at Postgres and a local mock OpenAI server, and walks 16 steps: miss, direct hit, semantic hit at 0.878, tenant isolation, eviction, TTL expiry, streaming replay, and a **gateway restart after which cached entries are still served** because they live in Postgres. The recorded run is [`e2e/e2e-run.log`](e2e/e2e-run.log) (re-run 10 Nov 2026, 16/16).

## Where things are

```
bifrost/              Bifrost fork (git submodule) with the pgvector store, branch feat/pgvector-vectorstore
freshness-companion/  the proxy, its tests, demo page, Tavily client, watcher (internal/watch), MCP server (internal/mcpserver)
dashboard/            Go server + single HTML page
eval-harness/         storebench, cachequality, hybridspike, impactbench + results/
e2e/                  end-to-end harness and the recorded run
sql/                  cache_insights views, install/load/test scripts
tools/mock-openai/    deterministic stand-in model and embeddings
docker/               compose configs, Dockerfiles, demo policy
DOCKER.md             the walkthrough judges should follow
```

## Honest limits

- Every benchmark number here uses a mock model and mock embeddings. They show the mechanics and the trade-off, not real-model quality or a savings forecast.
- The Tavily demo depends on a public copy of the demo page (GitHub Pages) and on GitHub Pages redeploying after an edit, which takes about a minute. Tavily's remembered copies are bypassed with a `_fresh=` URL stamp; a site that rejects unknown query parameters would need that switched off.
- The audit's default comparison is exact text. With a real model (Groq, 10 Nov) that flagged 2 of 2 correct paraphrase reuses as wrong, because real models word things differently each time. A `judge` mode, where a model decides if two answers agree, got both test cases right. Two cases is a test, not a validation.
- The gate covers hits below its line and fails open if it cannot run. Exact-hash hits on a pre-planted answer in a shared bucket are not gated.
- Row-level security guards against bugs and forgotten filters, not against someone holding the app role's password.
- The companion keeps its state in one process. Two copies would not share it. The store is benchmarked (10,000 rows, 8 clients); the whole gateway plus companion is not load-tested.
- Smart checking trades freshness for fetches, and the simulator says by how much: with a 5-minute cap, a page that changes every 10 minutes had 24% of its answers served stale (all within the cap). The simulator uses a fake page and Poisson traffic, not real users; the saving on real traffic depends on how often the page actually changes.
- Latency measured from a Windows host through Docker Desktop is dominated by Docker's port proxy (60 ms vs about 8 ms inside the network). Representative numbers in `eval-harness/` come from a Linux client on the Docker network.
- Discovery (opt-in) sends a short query to Tavily Search. It never contains the question or the answer, but it does reveal the topic (the product name), and the opt-in `proper_nouns` mode can send a person's name. In the live check search did not find the demo page at all; it is not measured for accuracy and only checks cached hits, not new answers. Categories, query templates and product names still have to be configured; only the per-fact URL and pattern go away.
- We measured a keyword-plus-meaning hybrid search and chose not to build it; the mock embeddings flatter it and it would cost privacy. See `eval-harness/results/hybridspike-recommendation.md`.

## Technologies

Go 1.27 · PostgreSQL 16 · pgvector 0.8 (`pgx` driver, binary protocol) · Bifrost (Go AI gateway) · Tavily Extract and Search APIs · Docker Compose · plain HTML/JS for the dashboard and demo page · PowerShell and Bash for the scripts.

## Team and tools

Built solo by **Madhusudhan M** ([MadhusudhanM23](https://github.com/MadhusudhanM23)) during the two-week hackathon window. AI coding assistants were used for drafting code, tests and documentation; everything was then verified by running it, and every number in this README comes from a recorded run that lives in this repository.

## Licence

Apache 2.0, same as Bifrost. See [LICENSE](LICENSE).
