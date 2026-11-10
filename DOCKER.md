# Running the whole demo with Docker

One command, on any machine with Docker installed — no Go toolchain, no prebuilt
`.exe` files, no Windows-specific paths:

```sh
docker compose up --build
```

(or `docker-compose up --build` on an older Compose install). First run builds five
images from source — the real Bifrost gateway from the local fork in `bifrost/`
(with the pgvector `VectorStore` backend, **not** the published Bifrost image, which
doesn't have it), the mock OpenAI server, and the freshness companion + demo page —
then starts Postgres+pgvector and waits for each service's healthcheck before
starting the next. The first build is slow: about 10 minutes on a 4-core desktop
(measured 604 s), because `go build` inside the containers downloads Go modules once.
Later starts take seconds.

To check that everything works, run the smoke test once the services are healthy
(PowerShell; it sends real requests through the companion and prints PASS/FAIL):

```sh
powershell -File smoke.ps1
```

Stop everything with `docker compose down` (add `-v` to also drop the Postgres
volume and start the cache empty next time).

## What's running and where

| Service | Container port | Host port | What it is |
|---|---|---|---|
| `postgres` | 5432 | `localhost:5434` | PostgreSQL 16 + pgvector (`pgvector/pgvector:pg16`) |
| `mock-openai` | 18080 | `localhost:18080` | stdlib-only mock OpenAI server (`tools/mock-openai`) |
| `bifrost` | 8080 | `localhost:18090` | the real gateway, built from `bifrost/` (branch `feat/pgvector-vectorstore`), semantic-cache plugin backed by pgvector |
| `demoserver` | 8000 | `localhost:8091` | serves `freshness-companion/demo-page/` with caching disabled |
| `freshness-companion` | 8090 | `localhost:8090` | the proxy you actually talk to |

Containers talk to each other by service name (`postgres`, `mock-openai`, `bifrost`,
`demoserver`) on the compose network; only `freshness-companion`'s port matters for
the demo below — everything goes through it.

## Evidence provider: offline by default

`freshness-companion` defaults to `-provider direct`: a plain HTTP GET of the demo
page, no API key needed, which is what `docker compose up` runs out of the box.

To switch to real Tavily evidence:

1. put `TAVILY_API_KEY=tvly-...` in a `.env` file next to `docker-compose.yml` (or
   `export TAVILY_API_KEY=tvly-...` before `docker compose up`),
2. in `docker-compose.yml`, change the `freshness-companion` service's
   `-provider=direct` to `-provider=tavily_extract`,
3. in `docker/policy.json`, change the fact's `source_url` from
   `http://demoserver:8000/` to a page Tavily can reach from the public internet —
   Tavily cannot fetch an address private to the compose network, so publish
   `freshness-companion/demo-page/index.html` somewhere public (e.g. GitHub Pages)
   first.

## Demo: miss → direct hit → semantic hit → stale-catch

Everything below goes through the companion on `localhost:8090`, which proxies to
Bifrost unchanged except on the one time-sensitive cache hit it has to re-check.
Run these from the `hackathon-workspace` root so the `@...` file paths resolve.

**1. Miss, then direct hit, then semantic hit** — reproducing the pgvector
semantic-cache proof from `e2e/e2e-run.log` (steps a/b/c), now running through the
companion and a real pgvector-backed Bifrost instead of the native Windows build:

```sh
# Miss: first time this is asked, Bifrost calls the mock model and writes one
# row (256-dim vector) to Postgres. x-bifrost-cache-hit: false.
curl.exe -s -D - http://localhost:8090/v1/chat/completions \
  -H "Content-Type: application/json" -H "x-bf-cache-key: demo-capital" \
  --data-binary "{\"model\":\"openai/gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"What is the capital city of France?\"}]}"

# Identical request: direct hit, model not called. x-bifrost-cache-hit-type: direct.
curl.exe -s -D - http://localhost:8090/v1/chat/completions \
  -H "Content-Type: application/json" -H "x-bf-cache-key: demo-capital" \
  --data-binary "{\"model\":\"openai/gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"What is the capital city of France?\"}]}"

# Paraphrase: semantic hit via pgvector cosine search, similarity above the 0.8
# threshold, model still not called. x-bifrost-cache-hit-type: semantic.
curl.exe -s -D - http://localhost:8090/v1/chat/completions \
  -H "Content-Type: application/json" -H "x-bf-cache-key: demo-capital" \
  --data-binary "{\"model\":\"openai/gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"Tell me the capital city of France.\"}]}"
```

(Dropping `curl.exe` for plain `curl` on macOS/Linux works the same way.) None of
these three questions are time-sensitive, so the freshness companion passes them
straight through — this part is exhibiting Bifrost's pgvector-backed semantic cache,
not the companion.

**2. Stale-catch** — the freshness companion's own proof. The demo page (served by
`demoserver`, bind-mounted from `freshness-companion/demo-page/index.html`) starts
at **$49**.

This step uses `docker/request-49.json` / `docker/request-59.json`, not
`freshness-companion/examples/request-49.json` directly: `tools/mock-openai`
only states a fact from the system prompt back in its answer when that prompt
uses its `Context:` list format (see `tools/mock-openai/answer.go`) — a plain
sentence like the one in `freshness-companion/examples/request-49.json` gets
echoed verbatim ("Mock answer to: ...") with no price in it at all, which the
companion correctly reports as `unable_to_verify` rather than `confirmed`.
`docker/request-49.json`/`-59.json` carry the same price-in-system-prompt idea,
reformatted as `Context:\n- The Acme Pro plan costs $49 per month.` so the mock's
answer actually contains the price:

```sh
# 1. Miss: Bifrost caches the model's "$49" answer.
curl.exe -s -D - http://localhost:8090/v1/chat/completions \
  -H "Content-Type: application/json" -H "x-bf-cache-key: demo" \
  --data-binary "@docker/request-49.json"

# 2. Ask again: a cache hit, and the page still says $49, so the companion
#    confirms it: x-freshness-status: confirmed, x-freshness-action: served.
curl.exe -s -D - http://localhost:8090/v1/chat/completions \
  -H "Content-Type: application/json" -H "x-bf-cache-key: demo" \
  --data-binary "@docker/request-49.json"
```

Now change the price — exactly the "edit the price, re-ask" step from the proven
manual demo, just against a bind-mounted file instead of a local one. Edit
`freshness-companion/demo-page/index.html` and change `$49` to `$59` in the
`#pro-plan-price` line, then wait for the evidence cache to expire (5s in this
compose setup) and ask again:

```sh
# 3. The companion sees x-freshness-status: stale (cached 49, current 59), clears
#    the stale entry, re-sends the request, and returns the model's fresh "$59"
#    answer. x-freshness-action: refreshed.
curl.exe -s -D - http://localhost:8090/v1/chat/completions \
  -H "Content-Type: application/json" -H "x-bf-cache-key: demo" \
  --data-binary "@docker/request-59.json"

# 4. What was decided, and why:
curl.exe -s "http://localhost:8090/freshness/decisions?limit=5"
```

Step 3 must come after the evidence cached in step 2 has expired (5 s here) or the
companion will still see the old page content and confirm `$49` again.

## Reading the receipt

Every answer that went through a freshness check carries one more header,
`x-freshness-receipt`: a single line that says what was checked, when, and what came of
it. The quickest way to see it is the demo script, which prints it in words:

```
powershell -File demo-ask.ps1 -Key receipt-demo
powershell -File demo-ask.ps1 -Key receipt-demo
```

```
Cache:      MISS (the model was asked)
Freshness:  status=confirmed  action=served  via=direct_fetch (not Tavily)
Checked:    http://demoserver:8000/
Receipt:    confirmed, found 49 at http://demoserver:8000/, checked 0s ago via direct_fetch (not Tavily) (fetched)
Answer:     According to the provided information, The Acme Pro plan costs $49 per month.

Cache:      HIT (answer came from the cache)
Freshness:  status=confirmed  action=served  via=direct_fetch (not Tavily)
Checked:    http://demoserver:8000/
Receipt:    confirmed, found 49 at http://demoserver:8000/, checked 0s ago via direct_fetch (not Tavily) (reused)
Answer:     According to the provided information, The Acme Pro plan costs $49 per month.
```

The first ask is a miss, but the demo policy checks new price answers too
(`verify_new_answers`), so it already has a receipt. The second ask is a hit; `(reused)`
says the companion did not read the page again but used the notes it kept from the first
look (they live for `-evidence-ttl`, 5 s in this stack), and `checked 0s ago` says how old
those notes were. After the `$49` to `$59` edit from the demo above, the receipt reads
`stale, found 59 at http://demoserver:8000/, checked 0s ago via direct_fetch (not Tavily) (fetched)`.
A request with no freshness check (a question that is not time-sensitive, or one the guard
kept out of the cache) prints `Receipt: none (no freshness check ran)`.

The raw header, for a client that wants to parse it:

```
curl -s -D - -o NUL http://127.0.0.1:8090/v1/chat/completions -H "content-type: application/json" -H "x-bf-cache-key: receipt-demo" --data @docker/request-49.json | findstr /i x-freshness-receipt
```

```
X-Freshness-Receipt: v=1; verdict=confirmed; action=served; found="49"; checked_ago_s=0; source="http://demoserver:8000/"; provider="direct_fetch (not Tavily)"; checked_at="2026-11-10T10:54:05Z"; evidence=fetched; stated="49"; fact="Acme Pro plan price"; answer=cached
```

`GET /freshness/explain/<x-freshness-request-id>` returns the same information as a `receipt`
object, and the dashboard shows the receipt in words under each Try-it step on the
companion's side and in the request log's Checks column. The answer body is never changed.
Format and keys: [freshness-companion/README.md](freshness-companion/README.md#answer-receipts).

## More demos: wrong answers that look right

The mock model uses `docker/mock-knowledge.json`, so questions with the same meaning get
the same answer and look-alike questions get different ones. The companion runs with the
audit on and `-verify-below=0.90` (the pre-serve gate); `docker/policy.json` also has a
`guard` section. Use any unique `x-bf-cache-key` so earlier runs do not interfere.

```sh
# A. Look-alike question. 1: cache the password answer. 2: ask about the USERNAME.
#    Bifrost matches it semantically (about 0.84), but the gate asks the model first,
#    sees a different answer and serves the right one: x-freshness-gate: blocked.
#    3: the username question now has its own exact entry.
curl.exe -s -D - http://localhost:8090/v1/chat/completions -H "Content-Type: application/json" -H "x-bf-cache-key: lookalike-1" -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"How do I reset my Acme account password from the mobile app?"}]}'
curl.exe -s -D - http://localhost:8090/v1/chat/completions -H "Content-Type: application/json" -H "x-bf-cache-key: lookalike-1" -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"How do I reset my Acme account username from the mobile app?"}]}'

# B. A request that carries a tool's result is never cached (x-freshness-guard).
curl.exe -s -D - http://localhost:8090/v1/chat/completions -H "Content-Type: application/json" -H "x-bf-cache-key: tool-1" -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"What is the status of my order?"},{"role":"tool","tool_call_id":"c1","content":"{\"status\":\"shipped\"}"}]}'

# C. Counters: gate results, audits, guard.
curl.exe -s http://localhost:8090/freshness/audits?limit=5
curl.exe -s http://localhost:8090/freshness/guard
```

```sh
# D. A verified (human-approved) answer: no model call, header x-freshness-verified.
curl.exe -s -D - http://localhost:8090/v1/chat/completions -H "Content-Type: application/json" -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"What are the support hours for Acme?"}]}'

# E. Text with an email address is kept out of the cache (x-freshness-guard: bypassed: sensitive:email).
curl.exe -s -D - http://localhost:8090/v1/chat/completions -H "Content-Type: application/json" -H "x-bf-cache-key: pii-1" -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"My email is jane@example.com, summarise my plan"}]}'

# F. Why did I get that answer? Use the x-freshness-request-id from any response.
curl.exe -s http://localhost:8090/freshness/explain/<request-id>
```

SQL views over the cache (needs PowerShell): `powershell -File sql\install.ps1`, then see `sql/README.md`.

Everything here uses the mock model and mock embeddings: it proves the mechanics, not
how a real model behaves. The dashboard (`dashboard/`, run on the host) shows these
numbers: `dashboard.exe -companion http://127.0.0.1:8090 -bifrost http://127.0.0.1:18090`.

## MCP mode

The companion can also offer its checks as **MCP tools**, and Bifrost can connect to it
as an MCP client. Then anything that talks to Bifrost's MCP gateway (an agent, a chat
client, `curl`) can ask "is this price still right?", "why did request X get that
answer?", "what do you watch?" or "the policy page changed" as tool calls, with the
companion's own policy and evidence behind them. Off by default; one extra overlay turns
it on:

```sh
docker compose -f docker-compose.yml -f docker-compose.mcp.yml up -d --build --wait
```

What the overlay changes, and why (details in `docker-compose.mcp.yml`):

- **The companion starts with `-mcp`** and serves `POST /mcp` (plain JSON-RPC over HTTP,
  MCP `2025-06-18`, no SSE). Tools: `check_freshness`, `explain_request`,
  `list_watched_sources`, `report_change`. Their arguments and example `curl` calls are
  in [`freshness-companion/README.md`](freshness-companion/README.md#mcp-mode).
- **Bifrost reads `docker/bifrost-config.mcp.json`** instead of `docker/bifrost-config.json`.
  It is the same file plus an `mcp` block (client `freshness` →
  `http://freshness-companion:8090/mcp`, `tools_to_execute: ["*"]`, re-sync every
  minute) and a **Postgres config store** in the same database, because Bifrost only
  registers MCP clients when it has a config store (SQLite is not possible: the image is
  built with `CGO_ENABLED=0`). The config store brings three side effects, all handled
  by the overlay:
  1. **Migrations at boot.** The first start runs the config-store migrations, which
     takes tens of seconds; Bifrost's healthcheck gets `start_period: 60s`.
  2. **The admin API is locked.** With a config store and no admin account, every
     `/api/*` call must carry `X-Bifrost-Setup-Token`. The companion uses that API to
     delete stale cache entries, so it is started with `-bifrost-admin-header=X-Bifrost-Setup-Token`
     and `BIFROST_ADMIN_AUTH` set to the token. The token is `BIFROST_SETUP_TOKEN`
     (default `demo-setup`; put `BIFROST_SETUP_TOKEN=...` in `.env` to change it).
  3. **Inference auth would be on.** A fresh config store defaults
     `enforce_auth_on_inference` to true; the config sets it to false so
     `/v1/chat/completions` stays open like in the default stack, and sets
     `source_of_truth: config.json` so the file, not the database, is what counts.
- **No start-order change.** Bifrost marks the companion's client unstable if it is not
  up yet and retries every 10 s, so the usual order (companion after Bifrost) is kept.

Check it (PowerShell: `Invoke-RestMethod` with the same bodies works too):

```sh
# 1. The companion's own MCP endpoint answers the handshake.
curl -s http://127.0.0.1:8090/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'

# 2. Bifrost shows the client connected with the four tools (give it 10-20 s after start).
curl -s http://127.0.0.1:18090/api/mcp/clients -H 'X-Bifrost-Setup-Token: demo-setup'

# 3. Through Bifrost's MCP gateway the tools are prefixed with the client name.
curl -s http://127.0.0.1:18090/mcp -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
curl -s http://127.0.0.1:18090/mcp -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"freshness-check_freshness","arguments":{"question":"How much does the Acme Pro plan cost per month?","answer":"$49 per month"}}}'

# 4. Bifrost's direct tool-execution endpoint (the shape a chat client's tool call has).
curl -s http://127.0.0.1:18090/v1/mcp/tool/execute -H 'Content-Type: application/json' \
  -d '{"id":"c1","type":"function","function":{"name":"freshness-list_watched_sources","arguments":"{}"}}'
```

The normal demo (`smoke.ps1`, the `$49` → `$59` walk-through above) works unchanged in
this mode: the proxy path is the same, only the cache-clear calls carry the setup token.
`report_change` through MCP needs the companion's change-notice token only if
`FRESHNESS_NOTICE_TOKEN` is set, which the compose files do not do. Note the flip side:
without that token, anyone who can reach Bifrost's `/mcp` can call `report_change`
(including `clear_cache_keys`), the same exposure `POST /freshness/changes` has on the
companion's port. With the token set, the Bifrost client config here uses `auth_type: none`,
so `report_change` through Bifrost is refused until you add the token as a header in
`docker/bifrost-config.mcp.json`; the three read-only tools keep working. Back to the default
stack: `docker compose up -d` (the Bifrost config-store tables stay in the Postgres
volume and are harmless; `docker compose down -v` removes them).

## Opt-in: smart checking and the background watcher

Both are off by default, and the "Try it" steps above assume they are off (step 3 waits
for the fixed 5 s evidence TTL). Turn them on from the shell or a `.env` file next to
`docker-compose.yml`:

```powershell
$env:CHECK_ADAPTIVE = "true"; $env:WATCH = "true"
docker compose up -d
```

Variables: `CHECK_ADAPTIVE` (default `false`), `CHECK_MAX` (longest re-check interval,
default `60s`), `WATCH` (default `false`), `WATCH_MAX_PER_HOUR` (default `120`; the
Tavily overlay uses `30`, since each watcher fetch is a Tavily call). Unset them (or set
them to `false`) and `docker compose up -d` is the documented stack again.

What to look at:

```sh
# 1. The schedule: one row per source with its interval, last value and next check.
curl.exe -s http://localhost:8090/freshness/watch

# 2. Ask the price question a few times, 5-10 s apart: interval_seconds grows 5 -> 10 -> 20 -> 40 -> 60.
curl.exe -s http://localhost:8090/v1/chat/completions -H "Content-Type: application/json" -H "x-bf-cache-key: watch-1" -d "@docker/request-49.json"

# 3. Edit freshness-companion/demo-page/index.html ($49 -> $59). Within the current interval the
#    watcher sees the change, deletes the cached $49 answers (decision record answer_of=watcher,
#    action=evicted) and the next ask is a miss that says $59 without waiting for a check.
curl.exe -s "http://localhost:8090/freshness/decisions?limit=3"

# 4. Check one source right now (bearer token needed when FRESHNESS_NOTICE_TOKEN is set).
curl.exe -s -X POST http://localhost:8090/freshness/watch/check -H "Content-Type: application/json" -d "{\"fact\":\"Acme Pro plan price\"}"
```

With smart checking on, a page change is noticed only at that page's next check, which can
be up to `CHECK_MAX` away: a quiet page is checked less often on purpose. Change notices
(`POST /freshness/changes`) still take effect at once, and in the simulation
(`eval-harness/results/checksim-20261110.md`) they were the only way to reach zero stale
answers; the watcher alone made stale answers rarer to wait for, not rarer to serve.

Two more things to know: turn `WATCH` on together with `CHECK_ADAPTIVE` (on its own the
watcher re-checks every few seconds and spends its hourly cap within minutes), and
`POST /freshness/watch/check` spends the same hourly cap, so set `FRESHNESS_NOTICE_TOKEN`
if anyone else can reach the companion (with the Tavily overlay each check is a Tavily call).

## Real model (Groq, OpenRouter or Grok)

1. Put a key in `.env` next to this file (it is git-ignored):
   `GROQ_API_KEY=...` (Groq, starts `gsk_`), `OPENROUTER_API_KEY=...` (OpenRouter,
   has free models), or `XAI_API_KEY=...` (Grok from xAI, starts `xai-`).
2. `docker compose -f docker-compose.yml -f docker-compose.real.yml up -d`
3. `powershell -File real-model-check.ps1` — asks a real model once (a miss) and again
   (a cache hit). Pick a model with `-Model groq/openai/gpt-oss-20b`,
   `-Model openrouter/nvidia/nemotron-3.5-lightning:free`, and so on. Free OpenRouter
   models are sometimes rate-limited upstream (429); try another one.

Answers come from the real model; embeddings still use the local mock for now. Mock models (`openai/gpt-4o-mini`) still work in this mode, so
`smoke.ps1` passes too. Plain `docker compose up -d` goes back to mock-only.

## Troubleshooting

- `docker compose ps` — all five services should show `healthy`.
- `docker compose logs bifrost` — if Bifrost never becomes healthy, this is almost
  always Postgres not being ready yet (the `depends_on: condition: service_healthy`
  should prevent this) or a build that picked up the published `vectorstore`
  package instead of the local fork — check the build logs for
  `go build ... bifrost/transports/bifrost-http` and confirm `GOWORK` was set.
- `docker compose down -v` then `docker compose up --build` for a fully clean run
  (drops the Postgres volume and the freshness companion's cache-age/decision
  volume too).
