# Bifrost freshness companion

A small HTTP proxy that sits in front of [Bifrost](https://github.com/maximhq/bifrost) (Maxim AI's open-source AI gateway). For **time-sensitive cache hits only**, it checks current evidence for the cached answer and refuses to serve the answer when it is stale.

This is a separate hackathon project. It is not part of Bifrost and uses only Bifrost's documented interfaces. It is a Go module that uses only the standard library.

## The problem: similarity is not freshness

Bifrost's semantic cache returns an earlier answer when a new prompt is similar enough to an old one. That saves money and time, but "similar prompt" does not mean "still true":

1. Monday: "How much is the Acme Pro plan?" The answer "The Pro plan costs $49 per month." is cached.
2. Tuesday: the price changes to $59.
3. Wednesday: the same question is a cache hit, and the user is told $49.

The companion catches step 3. When a cache hit is about something time-sensitive, such as a price, it checks a configured source page, sees that the price is now $59, and returns a fresh answer instead of the cached one.

## How it works

```
client ──► freshness companion :8090 ──► Bifrost :8080 ──► model provider
                 │       ▲
                 │       └── DELETE /api/cache/clear/{cacheId}   (refresh)
                 ▼
         evidence provider (Tavily Extract, or a plain GET for offline demos)
         receives ONLY the configured source URL
```

1. **Forward.** `/v1/*` goes to Bifrost unchanged: same method, path, query, headers and body. The companion never adds cache headers, so Bifrost caches exactly as it would without the companion.
2. **Record entry age.** Bifrost does not expose how old a cache entry is. On a miss, Bifrost reports the id it writes the new entry under (`x-bifrost-cache-id`). The companion records the time of every miss per id in `data/cache-ages.json`, so the record survives restarts. A later hit on that id is that old. If an id was never seen on a miss, its age is unknown. A miss sent with `x-bf-cache-no-store: true` is not recorded, because Bifrost does not write that entry.
3. **Classify.** Only `POST /v1/chat/completions` responses that Bifrost served from its cache (`x-bifrost-cache-hit: true`) are inspected. The last user message is matched against the keyword rules of each category in the policy. If no category matches, the response is not time-sensitive and passes through untouched. No evidence is fetched for it.
4. **Decide** (see below). If the policy has a **watched fact** for the category, the companion:
   1. takes the value from the cached answer (`choices[0].message.content`, `answer_pattern`);
   2. fetches the source page through the evidence provider, with a 60 s cache per URL;
   3. takes the current value from the page (`source_pattern`);
   4. compares the two deterministically (`exact`, `numeric` or `boolean`).
5. **Act and annotate.** The cached answer is served, or the entry is refreshed and the fresh answer is returned. The JSON body is never modified, so OpenAI clients keep working. Everything the companion adds is in `x-freshness-*` response headers. Each decision is appended to `data/decisions.jsonl`.

**Refresh** (default strategy `clear_and_resend`): delete the stale entry with Bifrost's documented `DELETE /api/cache/clear/{cacheId}`, then re-send the original request. That request misses, so Bifrost calls the model and writes a fresh entry. If the re-sent request is answered from the cache by a different, semantically similar entry, that entry is cleared too, up to `-refresh-max-attempts` times. The strategy is behind an interface. `-refresh resend_with_headers` swaps in a header-based bypass (see Configuration). A response that Bifrost served from its cache is never accepted as a refresh.

## The four outcomes

| `x-freshness-status` | Meaning | Action |
|---|---|---|
| `confirmed` | The value in the cached answer equals the current value in the evidence. | Serve the cached answer. |
| `stale` | The evidence states a different current value. | Refresh (clear + re-request) and return the fresh answer. |
| `unable_to_verify` | The check could not be completed: evidence fetch failed or timed out, a pattern matched nothing, the page or answer states conflicting values, or the entry is older than `max_age` with no watched fact to check it against. | Category `on_unverifiable`: `bypass` (refresh) or `serve_with_warning`. |
| `unknown` | No reliable metadata (the entry's age is unknown), or no check was performed. | `on_unknown` (default `auto`: revalidate against evidence when a watched fact applies, otherwise serve with a warning). |

Rules that keep the labels honest:

- **`confirmed` only ever comes from an evidence comparison.** A hit that is younger than `max_age` in a category with no watched fact is served, but it is labelled `unknown` with the reason "served on policy only … nothing was checked against evidence".
- A watched fact is always checked, whatever the entry's age. The evidence cache stops this from calling Tavily on every hit.
- With several watched facts, one `stale` fact makes the answer stale, and one unverifiable fact prevents `confirmed`.
- The cached answer is read **before** evidence is fetched. If the answer states no value, the provider is not called.
- If a refresh cannot produce a fresh answer, the companion **answers 502 rather than serve the stale answer**. For example, the clear call is rejected and the stale entry is served again. The response keeps its `x-freshness-*` headers and adds `x-freshness-action: refresh_failed` and `x-freshness-error`. If the re-sent request returns an error from Bifrost, that error is passed through instead.

### Response headers

Freshness headers are set only on time-sensitive cache hits. Misses and non-time-sensitive responses get none. Bifrost's own `x-bifrost-*` headers are kept.

| Header | When | Content |
|---|---|---|
| `x-freshness-status` | always | `confirmed`, `stale`, `unable_to_verify` or `unknown` |
| `x-freshness-action` | always | `served`, `served_with_warning`, `refreshed` or `refresh_failed` |
| `x-freshness-reason` | always | human-readable explanation, including the entry age |
| `x-freshness-category` | always | matched categories, comma-separated |
| `x-freshness-fact` | a fact was evaluated | fact name |
| `x-freshness-source` | a fact was evaluated | the fact's `source_url` |
| `x-freshness-provider` | evidence was fetched | `tavily_extract`, `tavily_search` or `direct_fetch (not Tavily)` |
| `x-freshness-checked-at` | evidence was fetched | when the evidence was fetched (RFC 3339). With the evidence cache this can be up to `-evidence-ttl` old. |
| `x-freshness-cached-value` / `x-freshness-current-value` | values were extracted | the two compared values |
| `x-freshness-age-seconds` | age known | tracked entry age |
| `x-freshness-warning` | `served_with_warning` | warning text |
| `x-freshness-refreshed-value` / `x-freshness-refreshed-check` | after a refresh | value in the fresh answer and `matches_current`, `differs_from_current` or `no_value`. Refreshing the cache does not make the model know the new value; this header shows whether it did. |
| `x-freshness-error` | `refresh_failed`, or a gate that could not check | why |
| `x-freshness-shared-refresh` | the request shared another request's refresh | `true` |
| `x-freshness-gate` | the pre-serve check ran | `verified`, `trusted`, `blocked` or `unverified` |
| `x-freshness-guard` | the request was kept out of the cache | `bypassed: tool_results`, `bypassed: side_effect_tool` or `bypassed: sensitive:<kind>` (set on any response, hit or miss) |
| `x-freshness-request-id` | **every** `POST /v1/chat/completions` response (hit, miss, guarded, blocked, refreshed, stream, error) | the id to give to `GET /freshness/explain/{id}` |
| `x-freshness-verified` | the request matched a verified answer | `human` (served from the verified table), or `stale_verified` (a verified answer exists but the source page contradicts it, so it was not served) |
| `x-freshness-verified-label` | as above, when the entry has a label | the entry's label |
| `x-freshness-verified-check` | a verified answer was served | `confirmed`, `unable_to_verify` or `not_checked` (see "Verified answers") |
| `x-freshness-answer-source` | the companion answered by itself | `verified` |

The request id is the one header that is not about freshness: it is added even to responses that carry no other `x-freshness-*` header.

## Privacy rules

- The evidence provider receives **only the fact's `source_url`**. Tavily Search also receives the URL's host as a domain filter. Prompts, cached answers, the caller's headers and the caller's credentials are never sent. Tavily Extract's optional `query` parameter is deliberately never used, because it would carry user intent.
- `TAVILY_API_KEY` is read from the environment and sent only as `Authorization: Bearer …`. It is never placed in a URL or logged, and it is redacted from any error text.
- The clear-endpoint credential (`BIFROST_ADMIN_AUTH`) is read from the environment and is never logged.
- Source URLs with embedded credentials are rejected when the policy loads.
- The decision log (file and endpoint) stores metadata and the **extracted values only**: no prompts and no answers. A test asserts this.
- `GET /freshness/explain` and `GET /freshness/verified` hold ids, scores, times, labels and hashes only, never a prompt or an answer text. The `sensitive` guard rule puts only the detector's name in logs, headers, stats and explanations, never the matched text.

## Configuration

### Policy file (JSON)

See [`examples/policy.example.json`](examples/policy.example.json). Unknown fields are rejected, so a typo cannot silently disable a check. `_comment` fields are allowed anywhere.

```jsonc
{
  "version": 1,
  "defaults": { "max_age": "15m", "on_unverifiable": "bypass", "on_unknown": "auto" },
  "categories": [
    { "name": "price", "keywords": ["price", "cost", "how much", "plan"], "max_age": "10m", "on_unverifiable": "bypass" }
  ],
  "facts": [
    {
      "name": "Acme Pro plan price",
      "category": "price",
      "keywords": ["pro"],                       // optional: only prompts that mention it
      "source_url": "https://YOUR-GITHUB-USERNAME.github.io/acme-pricing-demo/",
      "source_pattern": "Acme Pro plan[^$0-9]{0,20}\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*per month",
      "answer_pattern": "(?i)\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*(?:/\\s*mo(?:nth)?|per\\s+month|a\\s+month|monthly)\\b",
      "compare": "numeric"
    }
  ]
}
```

- **Categories.** Each category has `keywords` (required), plus optional `max_age`, `on_unverifiable` (`bypass` or `serve_with_warning`) and `on_unknown` (`auto`, `revalidate`, `serve_with_warning` or `bypass`). Settings a category leaves out come from `defaults`. Built-in defaults: `max_age` 10m, `on_unverifiable` `bypass`, `on_unknown` `auto`.
  - Without a `categories` key, the four built-in categories apply: `price`, `policy`, `schedule` and `availability`. The same applies with `-policy ""`.
  - If several categories match, the strictest settings win: the smallest `max_age`, `bypass` over `serve_with_warning`, and the strictest `on_unknown`. Facts of every matched category are checked.
- **Keywords.**
  - Matching ignores case.
  - A keyword must sit on word boundaries: `pro` matches "Pro plan" but not "product", and `price` does not match "priceless". Boundaries are ASCII-based.
  - A space inside a keyword matches any whitespace, so `how much` matches "how   much".
  - A trailing `*` matches any word ending: `refund*`.
- **Watched facts.**
  - `source_pattern` and `answer_pattern` are Go RE2 regular expressions with **exactly one capture group**, the value. Use `(?:…)` for other grouping.
  - `compare: "numeric"` ignores currency symbols and separators, so `$49` ≠ `$59` while `49.00` = `49` and `1,299` = `1299`. One comma followed by 1–2 digits is a decimal comma (`49,00`).
  - `compare: "exact"` (the default) compares after trimming, collapsing whitespace and case-folding.
  - `compare: "boolean"` reduces whatever text the pattern captures to `true`/`false` by scanning it (word-boundary, case-insensitive) for a small fixed list of true-signal words (`available`, `supported`, `online`, `enabled`, `active`, `open`, `in stock`, `true`, `yes`, `can`) and false-signal words/phrases (`unavailable`, `not available`, `no longer available`, `unsupported`, `offline`, `disabled`, `inactive`, `closed`, `out of stock`, `sold out`, `false`, `no`, and simple negations like `is not`/`isn't`/`cannot`). False signals are checked first, so a negation of a true word (`not available`, `unavailable`) is never mistaken for the positive word it contains. This is a deterministic word-list normalizer, not an NLP sentiment model — differently-worded yes/no answers ("Yes, live chat is available" vs. "live chat support is currently available") still compare equal, but anything outside the word list is `unable_to_verify`, same as a numeric pattern that matches nothing. See [`examples/policy.boolean-demo.json`](examples/policy.boolean-demo.json) and [`demo-page/availability.html`](demo-page/availability.html).
  - If a pattern captures different values in the same text, the result is "conflicting values", which counts as `unable_to_verify`.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:8090` | listen address |
| `-bifrost` | `http://localhost:8080` | Bifrost base URL |
| `-policy` | `examples/policy.example.json` | policy file (`""` = built-in categories, no facts) |
| `-provider` | `tavily_extract` | `tavily_extract`, `tavily_search` (experimental) or `direct` |
| `-tavily-url` | `https://api.tavily.com` | Tavily API base URL |
| `-tavily-depth` | `basic` | Extract `extract_depth`: `basic` or `advanced` |
| `-tavily-format` | `text` | `text` or `markdown` (text keeps patterns simple) |
| `-tavily-timeout` | `10s` | Extract's server-side `timeout` parameter (clamped to 1–60 s) |
| `-direct-raw` | `false` | direct provider: match against raw HTML instead of visible text |
| `-evidence-ttl` | `60s` | reuse evidence per source URL for this long |
| `-evidence-error-ttl` | `10s` | remember an evidence failure for this long (no hammering during outages) |
| `-evidence-timeout` | `15s` | timeout for one evidence fetch |
| `-max-evidence-bytes` | 2 MiB | largest evidence body |
| `-ages` | `data/cache-ages.json` | entry-age store (`""` = memory only) |
| `-ages-max` | `10000` | most entries tracked (oldest evicted) |
| `-decisions` | `data/decisions.jsonl` | decision log (`""` = memory only) |
| `-decisions-ring` | `1000` | decisions kept in memory for the endpoint |
| `-refresh` | `clear_and_resend` | or `resend_with_headers` |
| `-refresh-max-attempts` | `3` | entries cleared at most per refresh |
| `-refresh-set-header` | – | header set on the re-request, `"Name: value"` (repeatable) |
| `-refresh-drop-header` | – | header removed from the re-request (repeatable) |
| `-bifrost-admin-header` | `Authorization` | header that carries `BIFROST_ADMIN_AUTH` on the clear call |
| `-clear-timeout` | `5s` | timeout for the clear call |
| `-upstream-timeout` | `2m` | timeout for one buffered Bifrost call (also the response-header timeout) |
| `-max-request-bytes` / `-max-response-bytes` | 8 MiB | larger bodies pass through without checks |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `-changes` | `data/changes.json` | file that persists change notices (`""` = memory only) |
| `-settle-delay` | `150ms` | wait before deleting an entry Bifrost has only just written |
| `-refresh-share` | `2s` | requests that need the same refresh share one (see "Stampedes"); a finished refresh stays shared this long. Negative = only while it runs |
| `-audit-*`, `-audits` | off | wrong-match audit, see below |
| `-verify-below` | `0` (off) | check every semantic hit scoring below this against a fresh answer **before** serving it (see "Checking risky matches before serving") |
| `-verify-trust` | `1h` | how long a verified question/entry pair is served without another check |
| `-verify-max-per-minute` | `120` | cap on these checks (each is a model call); hits over the cap are served unchecked and say so |
| `-verified` | `""` (off) | JSON file of human-checked answers the companion gives itself (see "Verified answers") |
| `-verified-scope-header` | `x-bf-vk` | request header compared with an entry's `scope` |
| `-verified-reload` | `5s` | how often the verified file is checked for changes (a modification-time poll; works on Windows) |
| `-explain-ring` | `2000` | request explanations kept in memory for `/freshness/explain` (oldest dropped; negative = keep none) |

**Environment variables (secrets are never passed as flags):**

| Variable | Use |
|---|---|
| `TAVILY_API_KEY` | required for the Tavily providers |
| `FRESHNESS_NOTICE_TOKEN` | optional; when set, `POST /freshness/changes` needs `Authorization: Bearer <token>` |
| `BIFROST_ADMIN_AUTH` | optional full header value for the clear call, for example `Bearer <management API key>` or `Basic <base64 user:password>`, when Bifrost's management API requires auth |

**Example: header-based bypass instead of clearing.** Dropping the cache key disables Bifrost's caching for that one re-request when no `default_cache_key` is configured. The stale entry stays in Bifrost and is checked again on its next hit.

```powershell
.\freshness-companion.exe -refresh resend_with_headers -refresh-drop-header x-bf-cache-key
```

Extra headers can be added on top of the default strategy too, for example `-refresh-set-header "x-bf-cache-threshold: 1"`.

### Endpoints

- `/v1/*`: proxied to Bifrost.
- `GET /healthz`: liveness, plus the provider, the refresh strategy, policy counts and the number of tracked entries.
- `GET /freshness/decisions?limit=N`: recent decisions, newest first. Default 50, maximum 1000. The full history is in the JSONL file.
- `POST /freshness/changes`, `GET /freshness/changes?limit=N`: change notices (see below).
- `GET /freshness/audits?limit=N`: wrong-match audit results and pre-serve check results (see below).
- `GET /freshness/guard`: how many requests the cache guard kept out of the cache, per reason (counts only; reasons are `tool_results`, `side_effect_tool` or `sensitive:<kind>`, never any text).
- `GET /freshness/explain/{id}`: "why did I get this answer?" for one request (see below). `GET /freshness/explain?limit=N`: the newest ids with a one-line summary (default 50, maximum 500).
- `GET /freshness/verified`: the verified-answer table as counts, ids (hashes) and labels. No questions, no answers.

Each decision record also has `shared_refresh` (this request waited for a refresh another request ran) and `gate` (what the pre-serve check did) when they apply.

Each decision record has `time`, `cache_id`, `hit_type`, `similarity`, `category`, `fact`, `outcome`, `action`, `reason`, `source_url`, `provider`, `cached_value`, `current_value`, `checked_at`, `evidence_cached`, `age_seconds`, `refresh_strategy`, `refreshed_cache_id`, `refreshed_check`, `error` and `latency_ms`.

## Running it

You need Go 1.27.3 and a running Bifrost with the semantic cache plugin enabled. Clients must send `x-bf-cache-key` (or Bifrost must have a `default_cache_key`), otherwise Bifrost does not cache at all. See `docs/features/semantic-caching.mdx` in the Bifrost repository. On this Windows machine Go is not on `PATH` by default:

```powershell
$env:Path = "C:\Program Files\Go\bin;" + $env:Path
go build -o freshness-companion.exe .
```

### Offline demo with DirectFetch (no Tavily)

`-provider direct` is a plain HTTP GET of the source URL. It is labelled `direct_fetch (not Tavily)` in headers, logs and `/healthz`.

1. Serve the demo page locally with caching disabled, so edits are visible immediately:
   ```powershell
   go run ./cmd/demoserver -dir demo-page -addr 127.0.0.1:8000
   ```
2. Copy `examples/policy.example.json` to `examples/policy.local.json` and set the fact's `source_url` to `http://127.0.0.1:8000/`.
3. Start the companion. The short evidence TTL lets the demo show a price change right away:
   ```powershell
   .\freshness-companion.exe -provider direct -policy examples/policy.local.json -bifrost http://localhost:8080 -evidence-ttl 5s
   ```

### With Tavily

1. Publish `demo-page/index.html` on GitHub Pages. For example, create a public repository `acme-pricing-demo`, add the file at the root, then choose Settings → Pages → Deploy from a branch → `main` / root. Tavily cannot reach `127.0.0.1`.
2. Set the fact's `source_url` to `https://<your-user>.github.io/acme-pricing-demo/`.
3. Set the key in the environment, not on the command line:
   ```powershell
   $env:TAVILY_API_KEY = "tvly-..."   # PowerShell
   # export TAVILY_API_KEY=tvly-...   # bash
   .\freshness-companion.exe -provider tavily_extract
   ```

The companion refuses to start with a Tavily provider and no key. It never falls back silently to DirectFetch.

### Demo script: `$49` becomes `$59`

For the clearest demo, set `exclude_system_prompt: true` in Bifrost's semantic cache config. System messages are then left out of the cache key: the app's context can change while the cache key stays the same, which is exactly how stale answers happen. The two request files differ only in the system message. The Bash and PowerShell commands are the same; use `curl.exe` in PowerShell.

```powershell
# 1. The page says $49. First request: miss; Bifrost caches "$49".
curl.exe -s -D - http://localhost:8090/v1/chat/completions -H "Content-Type: application/json" -H "x-bf-cache-key: demo" --data-binary "@examples/request-49.json"

# 2. Ask again: a cache hit. The page still says $49, so you get
#    x-freshness-status: confirmed, x-freshness-action: served.
curl.exe -s -D - http://localhost:8090/v1/chat/completions -H "Content-Type: application/json" -H "x-bf-cache-key: demo" --data-binary "@examples/request-49.json"

# 3. Change the price: edit demo-page/index.html ($49 -> $59). The app's context changes too.
#    Bifrost's cache key does not change, so Bifrost would serve "$49" again.
#    The companion sees x-freshness-status: stale (cached 49, current 59), clears the entry,
#    re-sends the request, and returns the model's fresh "$59" answer.
#    You also get x-freshness-refreshed-check: matches_current.
curl.exe -s -D - http://localhost:8090/v1/chat/completions -H "Content-Type: application/json" -H "x-bf-cache-key: demo" --data-binary "@examples/request-59.json"

# 4. What was decided, and why:
curl.exe -s "http://localhost:8090/freshness/decisions?limit=5"
```

Step 3 must come after the evidence from step 2 has expired. Otherwise the companion reuses the `$49` evidence and confirms the old answer. Wait longer than `-evidence-ttl` (5 s in the offline setup, 60 s by default), or restart the companion. With Tavily and GitHub Pages, also allow time for Pages to publish the edit, since it is served through a CDN cache.

If Bifrost is left with `exclude_system_prompt: false`, use `request-49.json` again in step 3. The cached answer is still caught as stale. The model's fresh answer then still says $49, because its context says $49, and `x-freshness-refreshed-check: differs_from_current` reports that honestly.

## Wrong from the start: checking new answers

A cached answer can also be wrong from the first request: the model was given outdated context (for example a RAG document that still says `$39`) and the wrong answer is then cached for everyone. Existing hit checks catch it on its first reuse, but a refresh asks the same model with the same context, so it can be wrong again.

Set these per category (or in `defaults`) in the policy:

| Field | Default | Meaning |
|---|---|---|
| `verify_new_answers` | `false` | Also check the answer of a cache **miss** against the watched facts, before it is reused. Misses only; streams are not checked. |
| `on_contradicted` | `report` | What to do when a fresh answer states a different value than the source (a new answer, or an answer a refresh just fetched). |

`on_contradicted`:

- `report`: serve it, labelled `x-freshness-status: contradicted`. Nothing is deleted.
- `uncache`: serve it, labelled, and delete its cache entry so nobody else is served it (`x-freshness-action: uncached`).
- `correct`: delete the entry and ask the model once more with a system message in front of the conversation that quotes the verified value in the source's own words (the text the `source_pattern` matched, at most 200 characters, plus the source URL and check time). If that answer matches the source it is returned and cached (`x-freshness-action: corrected`); if it still disagrees it is handled as `uncache`.

With several matching categories the most active mode wins, and new answers are verified if any category asks.

New headers: `x-freshness-answer` (`cached` or `new`), `x-freshness-answer-value` (the value a new answer states), `x-freshness-correction` (`reported`, `uncached`, `corrected`), `x-freshness-corrected-value` and `x-freshness-corrected-check`. When a refresh still disagreed, `x-freshness-refreshed-value/check` describe the plain refresh and the `corrected-*` headers the corrected answer that was served.

Things to know:

- Bifrost writes a new entry in the background. A delete that arrives first would find nothing, so the companion waits `-settle-delay` (default 150 ms) before deleting, and `uncache` repeats the delete a little later. This is best effort. The guarantee is the hit-time check: a watched fact is checked on every hit, so a wrong entry that survives is caught on its next use.
- `correct` sends a short quote of the source page to the model through Bifrost. If that is not acceptable, use `uncache`.
- `correct` caches the corrected answer under the original request's key only when Bifrost has `exclude_system_prompt: true`, because the correction is a system message. Otherwise the original request keeps missing and is corrected each time.
- Both behaviours cost model calls (one more per correction).

## Change notices: "this changed"

`POST /freshness/changes` lets the system that owns a fact say it changed, instead of waiting for `-evidence-ttl` or `max_age`:

```json
{"fact": "Acme Pro plan price"}
{"category": "policy"}
{"source_url": "https://example.com/pricing"}
```

Send exactly one of the three, plus optional `"note"` and `"clear_cache_keys": ["demo"]` (also deletes every Bifrost entry under those keys, at most 10). Unknown fact, category or source URL answers 404; the source URL is compared ignoring scheme and host case, a trailing slash and the fragment.

Effects:

1. The cached evidence for the covered facts' source URLs is dropped, so the next check fetches the page again. (This closes the lag window described under Limitations.)
2. The notice is remembered (`data/changes.json`, kept across restarts). A hit on an entry **written before the notice** and covered by it is re-checked: a watched fact decides from the fresh evidence as usual; with no watched fact to check against, the entry is treated as stale and refreshed. Entries written after the notice, and requests in other categories, are untouched. The response carries `x-freshness-change-notice`.

A fact notice covers that fact; a category notice covers every request in that category; a source URL notice covers the facts read from that page. Set `FRESHNESS_NOTICE_TOKEN` to require `Authorization: Bearer <token>` on `POST`; without it the endpoint is open and the companion logs a warning. `GET /freshness/changes?limit=N` lists recent notices. A notice makes the companion ask again; it cannot make the source page, a CDN or Tavily's own cache update faster.

## Are reused answers right? The wrong-match audit

A semantic hit can match a question that only looks similar ("reset my password" served for "reset my username"). Bifrost reports the similarity but cannot tell whether the reused answer was right. With the audit on, a share of semantic hits is checked **after** the answer has been returned (the client never waits): the companion asks the model the same question again with a throwaway cache key and `x-bf-cache-no-store`, so the fresh answer can neither read nor write the cache, and compares it with the one that was served.

| Flag | Default | Meaning |
|---|---|---|
| `-audit-rate` | `0` | fraction (0-1) of semantic hits to audit |
| `-audit-near-margin` | `0` | also audit every semantic hit that scored within this margin above the threshold, where wrong matches gather |
| `-audit-max-per-minute` | `30` | cap, because every audit is a model call |
| `-audit-compare` | `exact` | `exact` or `judge` |
| `-audit-judge-model` | request's model | model for `judge` |
| `-audits` | `data/audits.jsonl` | audit log |

The audit is off unless `-audit-rate` or `-audit-near-margin` is set. Audited hits answer with `x-freshness-audit: <id>`; results are at `GET /freshness/audits` (counters and records: id, cache id, similarity, threshold, verdict `same`, `different` or `error`; never the prompt or the answers).

How answers are compared:

- `exact`: equal after lower-casing and dropping punctuation. Free and deterministic, but strict: two correct answers worded differently count as different, so for free-text models the rate is an **upper bound**. It suits deterministic or short answers.
- `judge`: a model is asked whether the two answers agree (`SAME` or `DIFFERENT`). It costs one more call per audit and sends the question and both answers to the model provider. The plumbing is covered by tests against a fake model. On 10 Nov 2026 it got 2 of 2 real-model cases right (Groq `openai/gpt-oss-20b`): a paraphrase judged same, a look-alike judged different. That is a small test, not a validation.

The dashboard turns the audit records into a wrong-match rate next to the hit rate, a suggested threshold (the smallest cutoff above the worst separable wrong match, with how many right matches it would lose), a note when wrong matches score so high that no threshold can separate them, and a warning when the rate climbs between windows of 10 audits. Small samples are labelled as early signals.

## Stampedes: one refresh for many requests

When a popular answer goes stale, many requests can arrive together. Without care each one would delete the entry and call the model, and they would trample each other: one request deletes the fresh entry another just wrote, and some callers get an error. The companion now runs **one** refresh per stale entry and caller; the others wait for it and get the same fresh answer (`x-freshness-shared-refresh: true`). A finished refresh stays shared for `-refresh-share` (default 2 s) so a request that read the stale entry just before the refresh finished does not start another.

What is shared: only requests with the same body and the same headers (virtual key, user, cache key, authorisation). One caller's answer is never handed to another.

A test fires 8 concurrent requests at a stale entry: before this change the result was 8 deletes, extra model calls and several 502 errors; now it is 1 delete, 1 model call and 8 correct answers. If the re-sent request comes back from cache under the id that was just cleared, the answer is accepted only if it matches the evidence (another request may have written the fresh answer first); otherwise the refresh fails as before.

Not covered: many simultaneous **misses** for one new question each call the model, because that happens inside Bifrost.

## Checking risky matches before serving

The audit above looks at a hit after the answer has gone out. The **gate** looks first. With `-verify-below 0.90` (and Bifrost's threshold at 0.80) every semantic hit that scored between the threshold and 0.90 is checked against a fresh answer before it is served. That band is where look-alike questions ("reset my username" matching "reset my password") and planted near-duplicates match.

For each such hit the companion asks the model the question again, with the caller's own cache key but only Bifrost's exact (`direct`) path, and compares the two answers (same `-audit-compare` modes):

- **Same** (`x-freshness-gate: verified`): the cached answer is served. The pair is trusted for `-verify-trust`, and the fresh answer is cached for this exact question, so the next ask is a plain exact hit.
- **Different** (`x-freshness-gate: blocked`): the fresh answer is served instead and cached for this exact question. The look-alike entry is **not** deleted (it may be right for its own question); it simply can never be served to this question again, because the question now has its own entry. Look-alike questions therefore settle instead of evicting each other.
- **Could not check** (`x-freshness-gate: unverified`, with `x-freshness-error`): the model call failed or `-verify-max-per-minute` was reached. The cached answer is served as it was. This is deliberately fail-open; do not rely on the gate while it reports unverified.

Concurrent requests for the same hit share one check. Results are written to the audit log with `gate: true` (a "different" verdict then means the wrong answer was blocked, not that it reached anyone) and counted in `GET /freshness/audits` as `gate_verified`, `gate_trusted`, `gate_blocked` and `gate_unverified`.

Limits, stated plainly:

- It only covers hits **below** `-verify-below`. A planted entry that scores above it, or an exact (direct) hit on a question an attacker pre-cached in a shared bucket, is not gated. Isolate tenants (Bifrost does this by default) and raise `-verify-below` toward 1.0 if you can afford the model calls.
- Every check is a model call. `exact` compare is strict: with a real model it flags correct reuses that are worded differently. `judge` has been tested on only 2 real-model cases.
- A blocked answer is cached for the exact question only (no embedding), so paraphrases of that question are matched against the older entries, which the gate checks again.
- If the caller sent `x-bf-cache-no-store`, nothing is stored; the verified pair is remembered instead.

## Cache guard: requests that must not be cached

Some requests have answers that must never be reused. With the policy's `guard` section the companion sends them to Bifrost with a cache key nothing is stored under and `x-bf-cache-no-store`, so they can neither be answered from the cache nor leave an answer in it. Streams are covered too (only headers change). All three rules are off by default (the third, `sensitive`, is described below).

```json
"guard": {
  "tool_results": "bypass",
  "side_effect_tools": ["create_*", "send_*", "delete_*", "update_*"]
}
```

- `tool_results: "bypass"`: a message with role `tool` or `function` means the answer is built from live tool data. A similarity search sees the same words with different data as a match.
- `side_effect_tools`: shell-style patterns, case-insensitive, matched against the names in `tools` / `functions`. Replaying an old text answer where the model should have called `create_ticket` would skip the action.

Guarded responses carry `x-freshness-guard: bypassed: <reason>`. `GET /freshness/guard` reports counts per reason (no prompts, no tool names). Bifrost already refuses to store responses that contain tool calls (`cache_tool_call_responses` is off by default) and skips long conversations (`conversation_history_threshold`, default 3 messages); the guard closes the remaining gap: a short request that carries a tool result, or that offers an action tool.

### Never cache sensitive text (`guard.sensitive`)

A third guard rule, off by default. With `"sensitive": "bypass"`, any request whose messages contain text of a sensitive kind is kept out of the cache in exactly the same way as the rules above (a cache key nothing is stored under, plus `x-bf-cache-no-store`), so such text can neither be answered from the cache nor be left in it. Every message is scanned, whatever its role (`system`, `user`, `assistant`, `tool`), whether `content` is a string or a list of parts. Streams are covered.

```json
"guard": {
  "sensitive": "bypass",
  "sensitive_kinds": ["email", "card", "api_key", "jwt", "bearer_token", "ssn", "iban", "phone"]
}
```

`sensitive_kinds` is optional; leave it out for all kinds. A name that is not in the list makes the policy fail to load.

| Kind | Finds | Tries not to flag |
|---|---|---|
| `email` | `name@domain.tld` | `@mention`, `meet @ 5pm`, `user@localhost` |
| `card` | 13-19 digits, together or in groups split by single spaces or dashes, that pass the Luhn check | order numbers and timestamps that fail Luhn, repeated digits (`0000...`) |
| `api_key` | `sk-...` (20+ characters), `AKIA` + 16, `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_` + 30+, `xoxb-`/`xoxa-`/`xoxp-`/`xoxr-`/`xoxs-` | `sk-learn`, `ask-me-anything-...` |
| `jwt` | `eyJ....` three dot-separated parts | `eyJ` on its own |
| `bearer_token` | `Bearer ` followed by 20+ token characters including a digit | "the bearer of responsibilities" |
| `ssn` | `123-45-6789` with the numbers the SSA never issues (000, 666, 9xx, group 00, serial 0000) ruled out | pieces of longer dashed numbers |
| `iban` | 15-34 characters, country code + check digits, mod-97 valid | IBAN-shaped text that fails mod 97 |
| `phone` | `+` followed by 8-15 digits (spaces, dots, dashes, brackets allowed); US `(415) 555-2671` and `415-555-2671` | prices, dates, versions, bare digit strings |

**Trade-offs, stated plainly.** The detectors are tuned for few false alarms, so some real data is missed: a phone number with no `+` and no US separators (`0412345678`), a card number with other separators, an IBAN in lower case, a key with an unusual prefix. Names, addresses and free-text secrets ("my password is hunter2") are not detected at all. A false alarm only costs a cache miss, but one is possible: a long digit string that happens to pass Luhn (about 1 in 10 of those that are 13-19 digits long, for instance an 18-digit reference) is treated as a card. Treat this as a safety net, not as data-loss prevention.

The reason is `sensitive:<kind>`, for example `x-freshness-guard: bypassed: sensitive:email`. **The matched text is never logged, put in a header, counted in stats or shown in an explanation**; only the kind name is. `GET /freshness/guard` therefore shows keys such as `sensitive:email`, with counts only. A test checks that a planted address and card number appear nowhere in the stats, the explain output, the decision list or any header.

**Speed.** Measured on this machine (12th Gen Intel i3-12100, Go 1.27.3, all kinds on, one 100 KB body): about 17-26 ms for ordinary text that contains numbers, dashes and at-signs but nothing sensitive, and about 5-8 ms for a body that is nothing but digit groups. These are single runs of `go test ./internal/sensitive -bench .` and vary between runs. The cost is paid only when `guard.sensitive` is `"bypass"`. A unit test fails if scanning 100 KB takes more than 500 ms.

## "Why did I get this answer?" (explain)

Every response to `POST /v1/chat/completions` carries `x-freshness-request-id: <id>`. Give the id to the companion to get a plain account of what happened:

```powershell
curl.exe -s -i http://localhost:8090/v1/chat/completions -H "content-type: application/json" -H "x-bf-cache-key: demo" `
  -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"How much does the Acme Pro plan cost per month?"}]}'
# ... x-freshness-request-id: req-3fa9c2d1-7
curl.exe -s http://localhost:8090/freshness/explain/req-3fa9c2d1-7
curl.exe -s "http://localhost:8090/freshness/explain?limit=10"
```

Example of a reply. This is an illustration of the shape, written by hand; the project runs on a mock model, and the values shown are what your policy's patterns capture (with the example policy that is `49`, without the dollar sign):

```json
{
  "id": "req-3fa9c2d1-7", "time": "2026-11-06T09:30:12Z", "http_status": 200,
  "how": "direct_hit", "cache_hit": true, "hit_type": "direct", "cache_id": "entry-1a2b3c4d",
  "age_seconds": 312,
  "freshness_status": "confirmed", "freshness_action": "served", "category": "price",
  "fact": "Acme Pro plan price", "answer_value": "49", "current_value": "49",
  "checked_at": "2026-11-06T09:30:10Z", "checked_ago_seconds": 2,
  "plain": "This answer came from the cache. The same question had been asked before. The saved answer is 5 minutes old. The source page, checked 2 seconds ago, still says 49, so the saved answer was served."
}
```

`how` is one of: `miss`, `direct_hit`, `semantic_hit` (with `similarity` and `threshold`), `bypassed_by_guard` (with `guard_reason`), `verified_answer`, `blocked_by_gate`, `refreshed`, `refresh_failed`, `corrected`, `uncached`, or `not_cached` (Bifrost sent no cache headers: no cache key, a stream, an error). Other fields appear when they apply: `cache_id`, `age_seconds`, `freshness_status`, `freshness_action`, `gate`, `audit_id`, `shared_refresh`, `change_notice`, `correction`, `refreshed_check`, `verified_label`, `verified_status`. `plain` is 2-4 sentences in simple words.

- **Privacy.** An explanation holds ids, scores, times and verdicts. It never holds a prompt or an answer. The one thing that comes from an answer is `answer_value` (and from the source page, `current_value`): the short value the policy's patterns pulled out, such as `$49`. A test checks that a distinctive prompt and the answer text appear in no explain output.
- **Where it comes from.** An explanation is built from the headers the companion actually sent back, so it cannot disagree with what the caller saw. That also means it covers every path. A stream carries only what its headers say (`streamed: true`).
- **Memory only.** The last `-explain-ring` (default 2000) explanations are kept in memory; the oldest are dropped, and all are lost on restart. An unknown, mistyped, evicted or pre-restart id gives `404` with a plain message. Only `POST /v1/chat/completions` gets an id; other `/v1/*` routes do not.

## Verified answers (known answers written by a person)

For questions where a person has already written the right answer, the companion can give that answer itself. Start it with `-verified answers.json`:

```json
[
  { "question": "What is your refund policy?", "answer": "Refunds are given within 30 days of purchase.", "label": "refunds-2026", "source": "legal team, 2026-10" },
  { "question": "Where is the office?", "answer": "12 Example Street.", "scope": "acme", "label": "acme-office" }
]
```

**What this is, honestly.** It is a table of curated answers *in front of* Bifrost. It is **not** written into Bifrost's cache (Bifrost has no API for writing a cache entry), and it does **not** do similarity matching. A question matches only if it is the same as a listed question after lower-casing, dropping punctuation and collapsing spaces (so "WHAT is your refund policy??" matches; "What is your refund policy for gift cards?" does not and goes to the model as usual).

- **What the caller gets.** A normal OpenAI `chat.completion` reply (id `chatcmpl-verified-...`, the request's `model` echoed back, `usage` all zeros), with `x-freshness-verified: human`, `x-freshness-verified-label` and `x-freshness-answer-source: verified`. The model is not called, nothing is cached, and no Bifrost cache headers appear.
- **Streams.** A `stream: true` request is answered with a small, valid server-sent-events stream (one chunk with the whole text, a closing `stop` chunk, `data: [DONE]`). Chosen over passing the request through because a streaming client expects a stream, and passing it through would call the model, which is what the table exists to avoid.
- **Order.** The cache guard comes first: a request that carries tool results, offers an action tool or holds sensitive text is never given a curated answer.
- **Customers (`scope`).** An entry with a `scope` is given only to requests whose scope header (`-verified-scope-header`, default `x-bf-vk`) has exactly that value. An entry without a scope is for everyone; when both match, the scoped one wins. The same question may be listed once per scope. Caution: `x-bf-vk` carries a Bifrost virtual key, so a file that uses it as a scope contains keys; protect the file, or use a non-secret header such as `-verified-scope-header x-tenant-id`.
- **Checking the file.** It is validated at startup (a bad file stops the companion): it must be a JSON array, unknown fields are rejected, `question` and `answer` must not be empty, the same normalised question and scope twice is rejected, the file is at most 1 MiB, 5000 entries, 8 KiB per answer. Errors name the entry number, never the text.
- **Reloading.** Every `-verified-reload` (default 5 s) the file's modification time and size are checked, and a changed file is loaded. This works on Windows (there is no `SIGHUP` there). A changed file that does not validate is rejected: the previous table stays in use and `last_reload_error` in `GET /freshness/verified` says why.
- **When the answer states a watched fact.** If the question falls in a policy category that has a watched fact (a price), the curated text is run through the same check as a new model answer (`Engine.CheckNewAnswer`) against the source page. If the page **contradicts** it, the entry is not served: the request carries on to the cache and the model as usual, and the response says `x-freshness-verified: stale_verified` (plus the label). The entry shows `"stale": true` in `GET /freshness/verified` until a later check agrees. If the check agrees the reply says `x-freshness-verified-check: confirmed`; if it cannot be run (source down, no value in the text) the entry is served and says `unable_to_verify`. Limit: this uses the policy's fact patterns, so it only catches what those patterns can read, and it costs an evidence fetch (shared with the evidence cache, `-evidence-ttl`) per such request.
- **Listing.** `GET /freshness/verified` returns `enabled`, `count`, `loaded_at`, `last_reload_error`, `served`, `stale_skipped` and, per entry, a 12-character hash id, the label, whether it is scoped and whether it is stale. No questions, answers, sources or scope values.

## Tests

All tests are offline and use only the standard library (`httptest`). They never call the real Tavily API.

```powershell
go vet ./...
go test ./... -count=1
```

They cover:

- the classifier;
- regex extraction, numeric and exact comparison, conflicts;
- all four outcomes and the policy actions, including that `confirmed` never happens without evidence;
- age tracking, persistence across a restart, eviction and a corrupt store;
- the Tavily Extract and Search clients against a fake server that returns the documented success, partial-success, 4xx/5xx and 422 shapes, with key-redaction checks;
- the evidence cache: TTL, error TTL, request coalescing, panics;
- full proxy runs against a fake Bifrost (hit and miss headers, a recorded clear endpoint) and a fake page whose price changes:
  - a fresh hit is served;
  - a stale hit triggers a clear and a re-request;
  - the unverifiable path follows the policy;
  - non-time-sensitive hits never fetch evidence;
  - streams pass through unchecked;
  - a failed refresh answers 502 rather than serving the stale answer;
  - unknown-age revalidation;
  - no-store misses are not tracked;
  - no prompt or answer text reaches the decision log;
  - the explain endpoint (every path gets an id, scores and verdicts are right, no prompt or answer text, ring eviction, 404s);
  - verified answers (served without the model or cache, exact normalised match, streams, scopes, a price answer the source page contradicts, hot reload, listing without text);
  - the sensitive-text guard (each detector against true positives and look-alikes, kind narrowing, streams, nothing leaked to stats, headers or explanations) and a 100 KB speed check.

Tavily API shapes implemented, as documented at [Extract](https://docs.tavily.com/documentation/api-reference/endpoint/extract) and [Search](https://docs.tavily.com/documentation/api-reference/endpoint/search):

- Extract:
  - Request: `POST https://api.tavily.com/extract`, `Authorization: Bearer <key>`, body `{"urls": ["<source_url>"], "extract_depth": "basic", "format": "text", "timeout": 10}`.
  - Response: `{"results": [{"url", "raw_content"}], "failed_results": [{"url", "error"}], "response_time", "request_id"}`.
  - Errors: `{"detail": {"error": "..."}}` (400, 401, 429 with `Retry-After`, 432, 433, 500).
- Search (experimental):
  - Request: `POST /search` with `{"query": "<source_url>", "search_depth": "basic", "max_results": 5, "include_domains": ["<host>"], "include_raw_content": "text"}`.
  - The companion uses the `raw_content` of the result whose `url` equals the source URL.
  - The 422 response uses `{"detail": [{"loc", "msg", ...}]}`.

## Limitations

- **Tavily is not a truth oracle.** The evidence is whatever the source page says, as extracted by Tavily, possibly from Tavily's own cache, a CDN cache or a stale page. `confirmed` means "matches the configured source at `x-freshness-checked-at`", nothing more.
- **The evidence cache leaves a window.** A change on the source page is noticed only once the cached evidence for it expires (`-evidence-ttl`, default 60 s). This trades a short lag for not calling Tavily on every hit.
- **Extraction depends on patterns.** If the page or the answer is worded differently, the pattern matches nothing. That gives `unable_to_verify`, never `confirmed`. The patterns must be maintained with the page. If the model states the price differently ("$49 a seat"), the answer pattern may not match.
- **Streams are not checked.** `stream: true` requests and `text/event-stream` responses pass through untouched. The companion only reads their cache headers to record entry ages. Bodies larger than the size limits, non-chat endpoints (`/v1/responses`, `/v1/embeddings`, …) and other gateway routes are not checked either.
- **Entry age is the companion's own record.** Entries written while the companion was not in the path, or before its age file existed, have an unknown age until their next miss. A request sent around the companion is invisible to it.
- **Classification is keyword-based.** It is deterministic and explainable, but it misses paraphrases that use no keyword and can over-match. Over-matching only costs an extra check.
- **Refresh costs a model call,** and with `bypass` a long evidence outage turns caching off for that category (the error TTL limits provider calls, not refreshes). `resend_with_headers` leaves the stale entry in Bifrost.
- **A refresh does not make the model correct.** The fresh answer comes from the model and its context. `x-freshness-refreshed-check` shows whether it now matches the evidence.
- **Tenant isolation is Bifrost's, not the companion's.** Bifrost scopes every entry to the caller's virtual key and user (a hard database filter, tested against real PostgreSQL in `bifrost/plugins/semanticcache/plugin_tenant_isolation_pgvector_test.go`). Requests with no identity share one bucket, and `disable_identity_scoping` turns the walls off. A cache hit is also faster than a miss, which can reveal that someone in the same shared bucket asked a question; the companion does not hide that.
- **Sensitive-text detection is a safety net.** Pattern-based, so unusual formats, names, addresses and free-text secrets are missed, and some digit strings that happen to pass Luhn are flagged as cards (see the trade-offs above).
- **Verified answers are a table, not cache entries.** Exact match only, no paraphrases; the freshness check on them works only for facts the policy can read; the table is reloaded by polling, so a change shows up within `-verified-reload`.
- **Explanations are in memory,** limited to the last `-explain-ring` requests, and are built from response headers, so they say what the caller was told, not more.
- **Endpoints are unauthenticated.** `/freshness/decisions` and `/healthz` share the proxy port. They expose cache ids, source URLs and extracted values, but no prompts. Keep the companion on a trusted network.
