# Hybrid search (keywords + meaning): recommendation

Written by hand next to the generated `hybridspike-hash-*.md` and `hybridspike-mock-openai-*.md`.
Data: 84 labelled pairs (42 should-share, 42 should-not-share), bag-of-words embeddings only.

## What the spike showed (and what it cannot show)

- With bag-of-words embeddings the vector alone cannot separate the groups: the look-alikes score
  HIGHER than the paraphrases (mock embedder: median cosine 0.667 for should-not-share vs 0.239 for
  should-share; hash embedder: 0.833 vs 0.272). The "best separation" threshold is 0.00, which means
  "serve everything" (precision 0.48 to 0.50). At the deployed 0.80 threshold no paraphrase is served at all.
  So this is not a test of vector search quality, and a real embedding model must be run before anyone
  quotes a vector-only number.
- The keyword check on its own, "reject when the two questions are the same sentence with 1 or 2 words
  swapped for other words" (k=2), with the vector filter off, on the same 84 pairs:
  - hash embedder and mock embedder give the same result because the rule does not use the vector:
    wrong matches kept 4 of 42 (3 with the synonym list), right matches lost 1 of 42 (2 with the synonym
    list); precision 0.911 / recall 0.976 / F1 0.943 without synonyms, 0.930 / 0.952 / 0.941 with them.
  - PostgreSQL full-text version (stemmed lexemes from `to_tsvector('english', ...)`): 3 wrong kept, 2 right
    lost, F1 0.941.
  - The strict version ("reject if ANY content word differs") serves 0 of 42 paraphrases. Paraphrases use
    other words, so a strict keyword rule destroys recall. Only a "near-duplicate with a swap" rule is usable.
  - The result depends on k: k=1 keeps 16 to 17 wrong matches, k=3 loses 6 right ones.
- Look-alikes it cannot catch: those that differ only in stopwords ("ship TO Canada" vs "ship FROM Canada",
  neg-09) and the ones where a synonym or stemming hides the difference (neg-25 in one variant, neg-26,
  neg-03 in the full-text variant).

## Why the numbers are probably too good

1. The dataset's look-alikes were written as "same sentence, one concept swapped" and its paraphrases as
   "reworded sentence". The swap rule is nearly the definition of the dataset, independent of any embedding.
2. Bag-of-words embeddings see the same words the keyword rule sees. A real model already pulls
   "export" and "import" apart somewhat and puts paraphrases much closer than these do, so the extra
   information a keyword check adds on top of it is smaller than shown here.
3. The synonym list was written after reading the paraphrase pairs. The ts_rank rows are tuned on the data.
4. 84 pairs, no held-out set, no confidence interval: one pair is about 2 points.

Honest summary: the keyword rule is a cheap way to catch the "one word swapped" family of look-alikes,
and on this data it would remove about 90 to 95 percent of them while losing 1 or 2 of 42 paraphrases. How
much of that is already caught by a real embedding model at the deployed threshold is UNKNOWN.

## Product constraint

A cache entry stores no question text. Verified in `bifrost/plugins/semanticcache/utils.go`
(`buildUnifiedMetadata`) and in the live table: the metadata keys are only `provider`, `model`,
`cache_key`, `from_bifrost_semantic_cache_plugin`, `expires_at`, `params_hash`, plus `response` and
`stream_chunks`. The question is only turned into the vector and into `params_hash`. The cached `response`
is the answer, not the question. A keyword check on a hit therefore has nothing to compare the new
question with, unless the entry also stores the question.

## Cheapest honest design (if it is ever built)

Do not store the question. Store a small "keyword fingerprint" next to the vector in the existing
metadata JSONB, for example `lex`: the sorted list of stemmed content-word hashes (a short salted hash per
word, a few dozen bytes), written when the entry is stored. On a semantic hit, hash the incoming question the
same way and apply the swap rule (each side has 1..2 words the other lacks, so reject and treat it as a miss).
Properties:
- no readable question text in the database; the hashes cannot be turned back into words without a
  dictionary attack on a small vocabulary, so call it "reduced exposure", not "anonymous". Use a per-tenant
  salt if tenants must not be linked;
- no new column, no full-text index, no change to the vector query; the check runs in the plugin after
  `GetNearest`, on the one returned row;
- cost: one extra metadata field per entry, one more rule evaluation per semantic hit, and a rule that
  needs a language-specific stopword list.
Storing the normalised question (needed for PostgreSQL `ts_rank` or `websearch_to_tsquery` at query time) is
simpler but puts customer text in the cache table: that is the privacy cost, and the cache is today
text-free by design. A PostgreSQL full-text index on that text would also only help for candidate search;
for validating one returned match no index is needed.

## Recommendation: do NOT build it yet

1. The spike cannot tell whether it helps with a real embedding model, and that is the whole question.
   First run `cachequality` with a real model (the dataset and harness are ready, an API key is needed) and
   look at which of the 42 look-alikes are still served at the chosen threshold. Only if a meaningful number
   survive (say more than 10 to 15 percent of the look-alikes) does a keyword check have something to remove.
2. The product already has two other defences against wrong matches that need no stored text: the
   pre-serve gate / wrong-match audit and a higher threshold. Compare against them first: the audit log
   already measures the wrong-match rate per similarity bucket (see `sql/README.md`, question 8).
3. If step 1 shows a real gap, build the hashed-fingerprint design above behind a config flag, off by
   default, and measure it again on a held-out set of real (privacy-reviewed) look-alikes, because this
   dataset is too close to the rule to be trusted.
