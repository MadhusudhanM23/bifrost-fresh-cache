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
