# Live check: "Tavily finds the source" (discovery)

Run on 10 Nov 2026 between 12:02 and 12:06 UTC (file name as planned: `discover-live-20261110.md`).
Real Tavily API (Search and Extract), mock model, real Bifrost with pgvector, all in Docker.
No other paid API was used. No key material appears below.

## Setup

- A throwaway compose project (`-p disccheck`) next to the normal stack, on its own ports
  (companion 8690, Bifrost 18690, demo page 8691, mock model 18680, Postgres 5440), using the
  existing `hackathon-workspace-bifrost` image. Torn down afterwards with `down -v`.
- Files: `docker-compose.yml` + `docker-compose.tavily.yml` + a scratch overlay (not committed).
  The key came from the project `.env` through Compose; it was only checked for presence
  (`echo ${#TAVILY_API_KEY}` inside the container printed `57`).
- Companion flags, as in `docker-compose.tavily.yml` plus: `-discover -discover-max-per-hour=4
  -discover-store=` (memory only) and `-evidence-ttl=10m` (instead of `5s`, so the "reuse" steps
  cost no extra Extract call).
- Policy: `docker/policy.discover.json` (the Tavily policy with no watched facts). Price category
  discover block: entities `Acme Pro`, `Acme Team`; query `{entities} plan price per month`;
  `include_domains: ["madhusudhanm23.github.io"]`, `require_domains: true`, numeric compare,
  `near_chars: 40`, `min_score: 0.5`.
- Requests: `docker/request-49.json` (system context says $49) through
  `demo-ask.ps1 -Companion http://127.0.0.1:8690`, and `docker/request-59.json` (context says $59)
  with `Invoke-WebRequest`. The public demo page said $49 and was not edited.

## Credits

Counted from the companion's own counters after each run: `stats.searches` from
`GET /freshness/discovered` (Tavily Search calls) and `evidence.provider_calls` from
`GET /freshness/watch` (Tavily Extract calls). Every call is counted as 1 credit, including one
Extract that failed (Tavily may not bill failed extractions, so the real figure can be 4).

| Run | Search calls | Extract calls | Credits (counted) |
|---|---|---|---|
| 1. policy.discover.json as shipped | 1 | 0 | 1 |
| 2. same + `candidate_urls` (step 6) | 1 | 1 | 2 |
| 3. Raspberry Pi 5, real world (step 7) | 1 | 1 (failed) | 2 |
| **Total** | **3** | **2** | **5** (ceiling was 10) |

## Run 1: search did not find the demo page

1. New key `disc49a`, request-49: **miss**, nothing checked, nothing searched (0 credits).
2. Same again: **hit**. Discovery searched once:
   - query sent: `Acme Pro plan price per month` (with `include_domains: ["madhusudhanm23.github.io"]`)
   - result: **0 results** (`"reason":"search returned no results"`, so no result hosts to report)
   - outcome: `x-freshness-discovery: no_source`; the hit was handled exactly as without
     discovery: `status=unknown action=served` ("served on policy only", age 9 s within max_age 10 m).
   - receipt: `v=1; verdict=unknown; action=served; discovery=no_source; query="Acme Pro plan price per month"; answer=cached`
   - search latency: the decision took 2,134 ms (the search).
3. Same again: the miss was remembered (negative cache), no new search, same fallback (0 credits).
4. New key `disc59a`, request-59 twice: miss, then hit with `discovery=no_source` (remembered) and
   `status=unknown action=served`. **The $59 answer was served**, exactly as it would be with
   discovery off: without a source page there is nothing to catch it with.

`GET /freshness/discovered` after run 1:

```json
{"searches_last_hour":1,"searches_cap_per_hour":4,"searches":1,"no_source":3,
 "sources":[{"category":"price","query":"Acme Pro plan price per month","no_source":true,"reason":"search returned no results"}]}
```

## Run 2: candidate URL fallback (step 6)

Same policy plus `"candidate_urls": ["https://madhusudhanm23.github.io/bifrost-fresh-cache/acme-pricing/"]`
(a scratch copy; the committed policy has none). Companion restarted (memory store, so it searched again).

**Search did not find it; the candidate fallback read it.** This is not discovery by search, and
every output says so (`found_by=candidate_url`).

1. Key `disc49b`: miss (0 credits).
2. Hit: 1 Search (again 0 results: `rejected_by_reason` stayed empty) + 1 Extract of the candidate URL.
   `status=confirmed action=served`, found 49, stated 49. Explanation (plain words):

   > This answer came from the cache. The same question had been asked before. No watched fact
   > covered this; Tavily Search for "Acme Pro plan price per month" found no usable page, so the
   > policy's candidate URL https://madhusudhanm23.github.io/bifrost-fresh-cache/acme-pricing/ was
   > read instead (search did not find it). The saved answer is 0 seconds old. The source page,
   > checked 0 seconds ago, still says 49, so the saved answer was served.

3. Hit again: 0 credits (source and evidence reused):

   ```
   x-freshness-receipt: v=1; verdict=confirmed; action=served; found="49"; checked_ago_s=6; source="https://madhusudhanm23.github.io/bifrost-fresh-cache/acme-pricing/"; provider="tavily_extract"; discovered=true; found_by=candidate_url; query="Acme Pro plan price per month"; checked_at="2026-11-10T12:04:32Z"; evidence=reused; stated="49"; fact="discovered: Acme Pro plan price per month"; answer=cached
   x-freshness-reason: no watched fact applies; Tavily Search for "Acme Pro plan price per month" found no usable page, so the policy's candidate URL https://madhusudhanm23.github.io/bifrost-fresh-cache/acme-pricing/ was read; fact "discovered: Acme Pro plan price per month": cached value 49 matches current value 49 (numeric compare); entry age 11s
   ```

4. Key `disc59b`, request-59 twice: miss, then hit, 0 credits (same source, evidence reused):
   - `x-freshness-status: stale`, `x-freshness-discovery: stale`, cached 59 vs current 49.
   - The refresh asked the model again; it still said $59 (its context says so):
     `x-freshness-refreshed-check: differs_from_current`.
   - The category says `on_contradicted: correct`, but a discovered page is never quoted to the
     model: it was handled as uncache: `x-freshness-action: uncached`, `x-freshness-correction: uncached`.
   - receipt: `v=1; verdict=stale; action=uncached; found="49"; checked_ago_s=7; source="https://madhusudhanm23.github.io/bifrost-fresh-cache/acme-pricing/"; provider="tavily_extract"; discovered=true; found_by=candidate_url; query="Acme Pro plan price per month"; checked_at="2026-11-10T12:04:32Z"; evidence=reused; stated="59"; served_check=differs_from_current; fact="discovered: Acme Pro plan price per month"; answer=cached`
   - explanation: "The cache had an earlier answer, but it was not served. No watched fact covered
     this; Tavily Search for "Acme Pro plan price per month" found no usable page, so the policy's
     candidate URL ... was read instead (search did not find it). It said 59 while the source page,
     checked 8 seconds ago, says 49, so it was out of date. The new model answer disagreed with the
     source page too, so it was returned with a warning and its cache entry was deleted."

`GET /freshness/discovered` after run 2:

```json
{"stats":{"searches_last_hour":1,"searches_cap_per_hour":4,"searches":1,"reused":2,"no_source":0,"candidate_used":1,"rejected_by_reason":{}},
 "sources":[{"category":"price","query":"Acme Pro plan price per month",
   "url":"https://madhusudhanm23.github.io/bifrost-fresh-cache/acme-pricing/","found_by":"candidate_url",
   "found_at":"2026-11-10T12:04:31Z","expires_at":"2026-11-11T12:04:31Z","uses":3,
   "last_outcome":"stale","last_value":"49"}]}
```

## Run 3: a real-world question (step 7)

Scratch policy: price category discover block with entities `Raspberry Pi 5`, query
`{entities} price`, `include_domains: ["raspberrypi.com"]`, `require_domains: true`, numeric.
Scratch request: system context "- The Raspberry Pi 5 8GB costs $80.", question
"How much does the Raspberry Pi 5 8GB cost?". The mock model answered
"According to the provided information, The Raspberry Pi 5 8GB costs $80."

- Miss, then hit. Search query sent: `Raspberry Pi 5 price`.
- Picked result #1, score 0.89:
  `https://www.raspberrypi.com/news/price-rises-for-some-16gb-raspberry-pi-products`
  ("Price rises for some 16GB Raspberry Pi products"); 2 other results were dropped for `low_score`.
- Tavily Extract of that URL failed: `tavily extract: source URL not extracted: Failed to fetch url`.
  (The companion adds a `_fresh=<unix>` parameter to Extract URLs; whether that or the site caused the
  failure was not tested, to stay within the credit ceiling.)
- **Verdict: `unable_to_verify`**, so the companion fell back to the behaviour without discovery:
  `status=unknown action=served` (served on policy only, age within max_age).
  Receipt: `v=1; verdict=unknown; action=served; discovery=unable_to_verify; found_by=tavily_search; query="Raspberry Pi 5 price"; rank=1; score=0.89; answer=cached`
- Even had the page been read, a news post about price rises is not the product's price page; the
  deterministic pattern would only have confirmed $80 if exactly one amount sat right after
  "Raspberry Pi 5" on one line. Nothing was tuned after this result.

## What this shows, and what it does not

- Privacy held in practice: the only texts sent to Tavily Search were `Acme Pro plan price per month`
  and `Raspberry Pi 5 price`; Extract received only the chosen URL (plus the `_fresh` stamp). The
  offline test `TestDiscoveryNeverSendsPromptOrAnswer` checks every recorded request body.
- Search did **not** find the demo page, so the "Tavily finds it" path was not demonstrated live
  for it. The verdict mechanics (confirmed, stale, trust downgrade, reuse at 0 credits) were shown
  live only through the `candidate_url` fallback.
- The real-world search picked a plausible but wrong kind of page and could not read it; the
  safety rule kept that from becoming `confirmed`.
- Three questions are a smoke test, not an accuracy measurement.
