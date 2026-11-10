package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func h(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// ---------------------------------------------------------------------------
// 0. Setup
// ---------------------------------------------------------------------------

func (r *runner) stepSetup() error {
	r.begin("0", "setup: config, pgvector container, fresh namespace table, start mock + gateway", true)
	if err := r.readConfig(); err != nil {
		return err
	}
	ver, err := r.psqlScalar("SELECT extversion FROM pg_extension WHERE extname = 'vector'")
	if !r.expect(err == nil && ver != "", "pgvector extension present in container %s, db %s (version %q)", r.opt.pgContainer, r.opt.pgDB, ver) {
		return fmt.Errorf("cannot query the pgvector container: %v %s", err, ver)
	}
	pgVer, _ := r.psqlScalar("SHOW server_version")
	r.note("PostgreSQL %s, pgvector %s", pgVer, ver)
	for _, addr := range []string{r.mockAddr, r.gwAddr} {
		if !r.expect(!portInUse(addr), "port %s is free", addr) {
			return fmt.Errorf("port %s is already in use; stop whatever listens there first", addr)
		}
	}
	if r.opt.fresh {
		r.psqlShow("DROP TABLE IF EXISTS " + r.table(r.namespace))
	}
	if err := r.startMock(); err != nil {
		return err
	}
	if err := r.startGateway(); err != nil {
		return err
	}

	// The plugin creates the namespace table in Init (CreateNamespace).
	reg := "'" + r.table(r.namespace) + "'::regclass"
	r.psqlShow(`\d ` + r.table(r.namespace))
	comment, _ := r.psqlScalar("SELECT obj_description(" + reg + ", 'pg_class')")
	r.expect(comment == "bifrost_vectorstore", "table %s created by the gateway, comment %q", r.table(r.namespace), comment)
	colType, _ := r.psqlScalar("SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = " + reg + " AND attname = 'embedding'")
	r.expect(colType == fmt.Sprintf("vector(%d)", r.dimension), "embedding column is %s (want vector(%d))", colType, r.dimension)
	hnsw, _ := r.psqlScalar(fmt.Sprintf("SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = '%s' AND indexdef ILIKE '%%USING hnsw%%vector_cosine_ops%%'", r.namespace))
	gin, _ := r.psqlScalar(fmt.Sprintf("SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = '%s' AND indexdef ILIKE '%%USING gin%%jsonb_path_ops%%'", r.namespace))
	if r.dimension <= 2000 {
		r.expect(hnsw == "1", "HNSW (vector_cosine_ops) index present for dimension %d", r.dimension)
	}
	r.expect(gin == "1", "GIN (jsonb_path_ops) metadata index present")
	active := gatewayLogLines(r.gw.logPath, "plugin status: semantic_cache")
	r.expect(len(active) == 1 && strings.Contains(active[0], "active"), "gateway log: %s", strings.Join(active, " | "))
	for _, line := range gatewayLogLines(r.gw.logPath, "connecting to vectorstore", "auth middleware", "ERR") {
		r.printf("  gateway log: %s\n", line)
	}
	s := r.stats()
	r.printStats("start", s)
	r.expect(s.Chat == 0 && s.Embed == 0, "mock counters start at zero")
	r.baseRows = r.rowCount(r.namespace)
	r.expect(r.baseRows == 0 || !r.opt.fresh, "namespace table starts with %d rows", r.baseRows)
	r.note("table %s %s, indexes hnsw=%s gin=%s", r.namespace, colType, hnsw, gin)
	r.end()
	if !r.cur.passed() {
		return fmt.Errorf("setup failed")
	}
	return nil
}

// ---------------------------------------------------------------------------
// a-e: hit and miss behaviour
// ---------------------------------------------------------------------------

func (r *runner) stepA() error {
	r.begin("a", "first request (x-bf-cache-key: demo) -> miss, one upstream call, row written to Postgres", true)
	before := r.stats()
	r.printStats("before", before)
	res := r.chat(qBase, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	after := r.stats()
	r.printStats("after", after)
	r.respA = res
	if !r.expect(res.Status == 200, "HTTP 200 (got %d)", res.Status) {
		r.end()
		return fmt.Errorf("the gateway did not answer the first request")
	}
	r.expect(res.hit() == "false", "x-bifrost-cache-hit: false (got %q)", res.hit())
	r.idA = res.cacheID()
	r.expect(isUUID(r.idA), "x-bifrost-cache-id is the id the entry is written under (%s)", r.idA)
	r.expect(after.Chat == 1, "mock chat_calls == 1 (got %d)", after.Chat)
	r.expect(after.Embed-before.Embed == 1, "one embedding call for the semantic lookup (got %d)", after.Embed-before.Embed)
	r.expect(res.Cache["x-bifrost-cache-provider-used"] == "openai" && res.Cache["x-bifrost-cache-model-used"] == r.embeddingModel,
		"embedding provider/model headers: %s/%s", res.Cache["x-bifrost-cache-provider-used"], res.Cache["x-bifrost-cache-model-used"])
	r.textA = r.lastEmbeddingInput()
	r.printf("  exact text the gateway sent to /v1/embeddings: %q\n", r.textA)
	n := r.waitRows(r.namespace, r.baseRows+1, 10*time.Second)
	r.expect(n == r.baseRows+1, "namespace table has %d row(s) after the async write (got %d)", r.baseRows+1, n)
	r.expect(r.rowExists(r.namespace, r.idA), "a row with id = x-bifrost-cache-id exists")
	r.showRows(r.namespace, "")
	stored := r.rowField(r.namespace, r.idA, cachedResponseIDExpr)
	r.expect(stored == res.ID, "the row stores completion %s (got %q)", res.ID, stored)
	dims := r.rowField(r.namespace, r.idA, "vector_dims(embedding)")
	r.expect(dims == strconv.Itoa(r.dimension), "the row stores a %d-dim embedding (got %q)", r.dimension, dims)
	r.note("hit=%s cache-id=%s chat_calls=%d embedding_calls=%d rows=%d", res.hit(), r.idA, after.Chat, after.Embed, n)
	r.end()
	if !isUUID(r.idA) {
		return fmt.Errorf("no cache id from step a")
	}
	return nil
}

func (r *runner) stepB() error {
	r.begin("b", "identical request -> hit, hit-type direct, no upstream call", true)
	before := r.stats()
	res := r.chat(qBase, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	after := r.stats()
	r.printStats("after", after)
	r.respB = res
	r.expect(res.hit() == "true", "x-bifrost-cache-hit: true (got %q)", res.hit())
	r.expect(res.Cache["x-bifrost-cache-hit-type"] == "direct", "x-bifrost-cache-hit-type: direct (got %q)", res.Cache["x-bifrost-cache-hit-type"])
	r.expect(res.cacheID() == r.idA, "x-bifrost-cache-id is the entry from (a)")
	r.expect(after.Chat == before.Chat, "chat_calls unchanged (%d)", after.Chat)
	r.expect(after.Embed == before.Embed, "no embedding call: the direct lookup runs before the semantic one")
	r.expect(res.ID == r.respA.ID && res.Content == r.respA.Content, "served the cached completion %s", r.respA.ID)
	r.note("hit=%s type=%s latency=%sms chat_calls=%d", res.hit(), res.Cache["x-bifrost-cache-hit-type"], res.Cache["x-bifrost-cache-hit-latency-ms"], after.Chat)
	r.end()
	return nil
}

func (r *runner) stepC() error {
	r.begin("c", "paraphrase sharing most words -> hit, hit-type semantic, similarity >= threshold", true)
	before := r.stats()
	rows0 := r.rowCount(r.namespace)
	res := r.chat(qParaphrase, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	after := r.stats()
	r.printStats("after", after)
	r.respC = res
	sim, ok := res.similarity()
	thr, _ := strconv.ParseFloat(res.Cache["x-bifrost-cache-threshold"], 64)
	r.expect(res.hit() == "true", "x-bifrost-cache-hit: true (got %q)", res.hit())
	r.expect(res.Cache["x-bifrost-cache-hit-type"] == "semantic", "x-bifrost-cache-hit-type: semantic (got %q)", res.Cache["x-bifrost-cache-hit-type"])
	r.expect(res.cacheID() == r.idA, "served the entry from (a)")
	r.expect(ok && sim >= r.threshold && sim < 1, "x-bifrost-cache-similarity %v >= threshold %v (and < 1: a real near-match)", sim, r.threshold)
	r.expect(thr == r.threshold, "x-bifrost-cache-threshold %v equals the configured threshold", thr)
	r.expect(after.Chat == before.Chat, "chat_calls unchanged (%d)", after.Chat)
	r.expect(after.Embed == before.Embed+1, "one embedding call (direct miss, then semantic search)")
	textC := r.lastEmbeddingInput()
	mockSim := r.mockCosine(r.textA, textC)
	r.printf("  stored text %q vs query text %q\n", r.textA, textC)
	r.expect(math.Abs(mockSim-sim) < 1e-5, "pgvector's similarity %.9f equals the mock's float64 cosine %.9f within 1e-5", sim, mockSim)
	r.expect(r.rowCount(r.namespace) == rows0, "a semantic hit writes nothing (rows stay %d)", rows0)
	r.note("hit=%s type=%s similarity=%v threshold=%v (mock cosine %.6f) chat_calls=%d", res.hit(), res.Cache["x-bifrost-cache-hit-type"], sim, thr, mockSim, after.Chat)
	r.end()
	return nil
}

func (r *runner) stepD() error {
	r.begin("d", "unrelated request -> miss, upstream call", true)
	before := r.stats()
	rows0 := r.rowCount(r.namespace)
	res := r.chat(qUnrelated, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	after := r.stats()
	r.printStats("after", after)
	r.expect(res.hit() == "false", "x-bifrost-cache-hit: false (got %q)", res.hit())
	r.expect(after.Chat == before.Chat+1, "chat_calls incremented %d -> %d", before.Chat, after.Chat)
	mockSim := r.mockCosine(r.textA, r.lastEmbeddingInput())
	r.expect(mockSim < r.threshold, "its similarity to the cached entry is %.4f (mock cosine), below %v", mockSim, r.threshold)
	n := r.waitRows(r.namespace, rows0+1, 10*time.Second)
	r.expect(n == rows0+1, "the new response was written (rows %d -> %d)", rows0, n)
	r.note("hit=%s chat_calls %d->%d similarity-to-(a)=%.4f rows=%d", res.hit(), before.Chat, after.Chat, mockSim, n)
	r.end()
	return nil
}

func (r *runner) stepE() error {
	r.begin("e", "same question as (a) with x-bf-cache-key: other -> no hit (keys isolate entries)", true)
	before := r.stats()
	rows0 := r.rowCount(r.namespace)
	res := r.chat(qBase, chatOpts{headers: h("x-bf-cache-key", keyOther)})
	after := r.stats()
	r.printStats("after", after)
	r.expect(res.hit() == "false", "x-bifrost-cache-hit: false (got %q)", res.hit())
	r.expect(res.cacheID() != r.idA && isUUID(res.cacheID()), "a different cache id (%s): the key is part of the direct id", res.cacheID())
	r.expect(after.Chat == before.Chat+1, "chat_calls incremented %d -> %d", before.Chat, after.Chat)
	n := r.waitRows(r.namespace, rows0+1, 10*time.Second)
	r.expect(n == rows0+1, "an entry was written under key %q (rows %d -> %d)", keyOther, rows0, n)
	r.showRows(r.namespace, "")
	r.note("hit=%s cache-id=%s chat_calls %d->%d", res.hit(), res.cacheID(), before.Chat, after.Chat)
	r.end()
	return nil
}

// ---------------------------------------------------------------------------
// f. clearing
// ---------------------------------------------------------------------------

func (r *runner) stepF() error {
	r.begin("f", "DELETE /api/cache/clear/{cacheId} for the entry from (a) -> the same request misses", true)
	r.printf("  In this setup config_store is disabled, so the server installs no auth middleware: /api routes get\n")
	r.printf("  AuthBypassedMiddleware, which checks no credential and only refuses browser requests whose Origin header\n")
	r.printf("  is foreign (not localhost, not same-origin, not in client.allowed_origins). See x5 for a config-store setup.\n")
	url := r.gwURL + "/api/cache/clear/" + r.idA

	foreign := r.api("DELETE", url, h("Origin", "https://evil.example.com"))
	r.expect(foreign.Status == 403, "a request with a foreign Origin is refused with 403 (got %d)", foreign.Status)
	r.expect(r.rowExists(r.namespace, r.idA), "the entry is still there after the refused call")

	plain := r.api("DELETE", url, nil)
	r.expect(plain.Status == 200 && strings.Contains(plain.Body, "Cache cleared successfully"), "no credential needed: 200 Cache cleared successfully (got %d)", plain.Status)
	r.expect(!r.waitRowState(r.namespace, r.idA, false, 5*time.Second), "row %s deleted from Postgres", r.idA)
	r.showRows(r.namespace, "")

	before := r.stats()
	res := r.chat(qBase, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	after := r.stats()
	r.printStats("after", after)
	r.expect(res.hit() == "false", "the same request now misses (x-bifrost-cache-hit: %s)", res.hit())
	r.expect(after.Chat == before.Chat+1, "chat_calls incremented %d -> %d", before.Chat, after.Chat)
	r.expect(res.cacheID() == r.idA, "written back under the same deterministic direct id %s", r.idA)
	r.expect(res.ID != r.respA.ID, "a fresh completion %s (the deleted entry held %s)", res.ID, r.respA.ID)
	r.refreshedRespID = res.ID
	present := r.waitRowState(r.namespace, r.idA, true, 10*time.Second)
	r.expect(present, "row %s exists again", r.idA)
	deadline := time.Now().Add(5 * time.Second)
	stored := r.rowField(r.namespace, r.idA, cachedResponseIDExpr)
	for stored != res.ID && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		stored = r.rowField(r.namespace, r.idA, cachedResponseIDExpr)
	}
	r.expect(stored == res.ID, "the row now stores the fresh completion %s (got %q)", res.ID, stored)

	byKey := r.api("DELETE", r.gwURL+"/api/cache/clear-by-key/"+keyOther, nil)
	left, _ := r.psqlScalar(fmt.Sprintf("SELECT count(*) FROM %s WHERE metadata->>'cache_key' = '%s'", r.table(r.namespace), keyOther))
	r.expect(byKey.Status == 200 && left == "0", "DELETE /api/cache/clear-by-key/%s -> %d, rows left under that key: %s", keyOther, byKey.Status, left)
	r.showRows(r.namespace, "")
	r.note("auth: none (no config store => AuthBypassedMiddleware; foreign Origin -> 403); clear -> %d; re-request miss, same id, new completion %s; clear-by-key -> %d", plain.Status, res.ID, byKey.Status)
	r.end()
	return nil
}

// ---------------------------------------------------------------------------
// g. bypass / refresh
// ---------------------------------------------------------------------------

func (r *runner) stepG() error {
	r.begin("g", "ways to bypass or refresh one cached entry, and what each writes back", true)
	demo := h("x-bf-cache-key", keyDemo)

	r.printf("\n  g1) DELETE /api/cache/clear/{id} then re-request (performed in f)\n")
	before := r.stats()
	res := r.chat(qBase, chatOpts{headers: demo})
	after := r.stats()
	r.expect(res.hit() == "true" && res.Cache["x-bifrost-cache-hit-type"] == "direct" && res.ID == r.refreshedRespID && after.Chat == before.Chat,
		"g1: the next identical request is a direct hit serving the refreshed completion %s (not the original %s)", r.refreshedRespID, r.respA.ID)

	r.printf("\n  g2) x-bf-cache-type: semantic + x-bf-cache-threshold: 1.5 (the gateway clamps it to 1.0)\n")
	semOnly := h("x-bf-cache-key", keyDemo, "x-bf-cache-type", "semantic", "x-bf-cache-threshold", "1.5")
	before = r.stats()
	res = r.chat(qBase, chatOpts{headers: semOnly})
	after = r.stats()
	sim, _ := res.similarity()
	r.expect(res.hit() == "true" && res.Cache["x-bifrost-cache-hit-type"] == "semantic" && sim == 1 && res.Cache["x-bifrost-cache-threshold"] == "1" && after.Chat == before.Chat,
		"g2a: an IDENTICAL prompt is still served (semantic hit, similarity %v, threshold %s): its embedding is identical, cosine distance 0 <= 1 - 1.0, so this does not bypass", sim, res.Cache["x-bifrost-cache-threshold"])
	rows0 := r.rowCount(r.namespace)
	before = r.stats()
	res = r.chat(qParaphrase, chatOpts{headers: semOnly})
	after = r.stats()
	r.semanticOnlyID = res.cacheID()
	textPara := r.lastEmbeddingInput()
	r.expect(res.hit() == "false" && after.Chat == before.Chat+1, "g2b: a paraphrase misses (similarity %.4f < 1.0) and goes upstream", r.mockCosine(r.textA, textPara))
	r.expect(isUUID(r.semanticOnlyID) && r.semanticOnlyID != r.idA, "g2b: written under a NEW id %s (the request id: semantic-only requests skip the direct id)", r.semanticOnlyID)
	n := r.waitRows(r.namespace, rows0+1, 10*time.Second)
	r.expect(n == rows0+1 && r.rowExists(r.namespace, r.semanticOnlyID) && r.rowExists(r.namespace, r.idA), "g2b: a second row was added next to %s (rows %d -> %d); nothing was replaced", r.idA, rows0, n)
	r.showRows(r.namespace, fmt.Sprintf(" WHERE metadata->>'cache_key' = '%s'", keyDemo))
	res = r.chat(qParaphrase, chatOpts{headers: demo})
	sim, _ = res.similarity()
	r.expect(res.hit() == "true" && res.cacheID() == r.semanticOnlyID && sim > 0.9999,
		"g2c: a later normal paraphrase request is now served from the g2b row (similarity %v), not from %s", sim, r.idA)

	r.printf("\n  g3) omit x-bf-cache-key (no default_cache_key is configured)\n")
	before = r.stats()
	rows0 = r.rowCount(r.namespace)
	res = r.chat(qBase, chatOpts{})
	after = r.stats()
	r.expect(len(res.Cache) == 0, "g3: no x-bifrost-cache-* headers at all (no lookup ran)")
	r.expect(after.Chat == before.Chat+1 && after.Embed == before.Embed, "g3: went upstream (chat_calls %d -> %d), no embedding call", before.Chat, after.Chat)
	time.Sleep(700 * time.Millisecond)
	rows1 := r.rowCount(r.namespace)
	r.expect(rows1 == rows0, "g3: nothing written (rows %d -> %d)", rows0, rows1)
	res = r.chat(qBase, chatOpts{headers: demo})
	r.expect(res.hit() == "true" && res.ID == r.refreshedRespID, "g3: the cached entry is untouched: the next keyed request still serves %s", r.refreshedRespID)

	r.printf("\n  g4) x-bf-cache-no-store: true\n")
	before = r.stats()
	res = r.chat(qBase, chatOpts{headers: h("x-bf-cache-key", keyDemo, "x-bf-cache-no-store", "true")})
	after = r.stats()
	r.expect(res.hit() == "true" && after.Chat == before.Chat, "g4a: no-store still SERVES a cached hit (it only skips writes)")
	before = r.stats()
	rows0 = r.rowCount(r.namespace)
	res = r.chat(qNoStore, chatOpts{headers: h("x-bf-cache-key", keyDemo, "x-bf-cache-no-store", "true")})
	after = r.stats()
	noStoreID := res.cacheID()
	time.Sleep(700 * time.Millisecond)
	rows1 = r.rowCount(r.namespace)
	r.expect(res.hit() == "false" && after.Chat == before.Chat+1 && rows1 == rows0 && !r.rowExists(r.namespace, noStoreID),
		"g4b: a no-store miss goes upstream and writes nothing (rows %d -> %d; cache id %s never appears)", rows0, rows1, noStoreID)
	res = r.chat(qNoStore, chatOpts{headers: demo})
	r.expect(res.hit() == "false", "g4c: the same prompt without no-store misses again (nothing had been stored)")
	n = r.waitRows(r.namespace, rows0+1, 10*time.Second)
	r.expect(n == rows0+1, "g4c: and is written this time (rows %d -> %d)", rows0, n)

	r.note("refresh = clear-by-id + re-request (same id, new completion); bypass without read or write = omit x-bf-cache-key; semantic+threshold>1 still hits identical prompts and adds a second row on a paraphrase miss; no-store still serves hits")
	r.end()
	return nil
}

// ---------------------------------------------------------------------------
// h. persistence across a restart
// ---------------------------------------------------------------------------

func (r *runner) stepH() error {
	r.begin("h", "restart the gateway -> cached entries are still served from Postgres", true)
	rowsBefore := r.rowCount(r.namespace)
	r.printf("  stopping gateway pid %d (process kill, no graceful shutdown)\n", r.gw.pid())
	r.gw.stop()
	r.expect(waitPortClosed(r.gwAddr, 15*time.Second), "gateway port %s closed", r.gwAddr)
	r.expect(r.rowCount(r.namespace) == rowsBefore, "rows in Postgres while the gateway is down: %d", rowsBefore)
	if err := r.startGateway(); err != nil {
		return err
	}
	for _, line := range gatewayLogLines(r.gw.logPath, "plugin status: semantic_cache", "connecting to vectorstore", "namespace") {
		r.printf("  gateway log: %s\n", line)
	}
	active := gatewayLogLines(r.gw.logPath, "plugin status: semantic_cache - active")
	r.expect(len(active) == 1, "semantic_cache active after restart: CreateNamespace accepted the existing table (dimension and ownership comment match)")
	r.expect(r.rowCount(r.namespace) == rowsBefore, "rows unchanged by the restart (%d)", rowsBefore)

	before := r.stats()
	res := r.chat(qBase, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	r.expect(res.hit() == "true" && res.Cache["x-bifrost-cache-hit-type"] == "direct" && res.cacheID() == r.idA && res.ID == r.refreshedRespID,
		"identical request: direct hit on %s serving %s", r.idA, r.refreshedRespID)
	res2 := r.chat(qAfterBoot, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	sim, _ := res2.similarity()
	r.expect(res2.hit() == "true" && res2.Cache["x-bifrost-cache-hit-type"] == "semantic" && sim >= r.threshold,
		"new paraphrase %q: semantic hit (similarity %v, entry %s) from the pgvector cosine search (query plan: see x6)", qAfterBoot, sim, res2.cacheID())
	after := r.stats()
	r.printStats("after", after)
	r.expect(after.Chat == before.Chat, "no upstream chat calls after the restart (chat_calls %d)", after.Chat)
	r.note("restart ok; direct hit %s; semantic hit similarity=%v; chat_calls unchanged=%d", res.ID, sim, after.Chat)
	r.end()
	return nil
}

// ---------------------------------------------------------------------------
// i. metadata in the JSON body
// ---------------------------------------------------------------------------

func (r *runner) stepI() error {
	r.begin("i", "where cache metadata appears in the HTTP JSON body (besides the headers)", true)
	cases := []struct {
		label string
		res   *chatResult
	}{{"miss (a)", r.respA}, {"direct hit (b)", r.respB}, {"semantic hit (c)", r.respC}}
	for _, c := range cases {
		if c.res == nil {
			r.expect(false, "%s: no response recorded", c.label)
			continue
		}
		pretty, _ := json.MarshalIndent(c.res.CacheDebug, "      ", "  ")
		r.printf("  %s -> body.extra_fields.cache_debug = %s\n", c.label, pretty)
		if !r.expect(c.res.CacheDebug != nil, "%s: present at extra_fields.cache_debug", c.label) {
			continue
		}
		cd := c.res.CacheDebug
		r.expect(fmt.Sprint(cd["cache_hit"]) == c.res.hit(), "%s: cache_debug.cache_hit (%v) == x-bifrost-cache-hit (%s)", c.label, cd["cache_hit"], c.res.hit())
		r.expect(cd["cache_id"] == c.res.cacheID(), "%s: cache_debug.cache_id == x-bifrost-cache-id", c.label)
		if ht := c.res.Cache["x-bifrost-cache-hit-type"]; ht != "" {
			r.expect(cd["hit_type"] == ht, "%s: cache_debug.hit_type == x-bifrost-cache-hit-type (%s)", c.label, ht)
		}
		if sim, ok := c.res.similarity(); ok {
			v, _ := cd["similarity"].(float64)
			r.expect(v == sim, "%s: cache_debug.similarity (%v) == x-bifrost-cache-similarity (%v)", c.label, v, sim)
		}
	}
	r.note("JSON body: extra_fields.cache_debug on the native /v1 route (cache_hit, cache_id, hit_type, requested_provider/model, provider_used, model_used, input_tokens, threshold, similarity, cache_hit_latency); streams: last chunk only")
	r.end()
	return nil
}

// ---------------------------------------------------------------------------
// Extra checks
// ---------------------------------------------------------------------------

func (r *runner) stepX1DirectOnly() error {
	r.begin("x1", "x-bf-cache-type: direct stores a metadata-only row (NULL embedding, pgvector RequiresVectors() == false)", false)
	before := r.stats()
	res := r.chat(qDirectOnly, chatOpts{headers: h("x-bf-cache-key", keyDemo, "x-bf-cache-type", "direct")})
	after := r.stats()
	id := res.cacheID()
	r.expect(res.hit() == "false" && after.Chat == before.Chat+1 && after.Embed == before.Embed, "miss, one upstream call, no embedding call")
	r.expect(r.waitRowState(r.namespace, id, true, 10*time.Second), "row %s written", id)
	r.expect(r.rowField(r.namespace, id, "embedding IS NULL") == "t", "row %s has embedding NULL", id)
	r.expect(r.rowField(r.namespace, id, "metadata ? 'params_hash'") == "t", "row keeps params_hash (a real direct entry, not a placeholder)")
	r.showRows(r.namespace, fmt.Sprintf(" WHERE id = '%s'", id))
	res = r.chat(qDirectOnly, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	r.expect(res.hit() == "true" && res.Cache["x-bifrost-cache-hit-type"] == "direct" && res.cacheID() == id, "a default request for the same prompt is a direct hit on it")
	before = r.stats()
	res = r.chat(qDirectPara, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	after = r.stats()
	wouldBe := r.mockCosine("user: "+strings.ToLower(qDirectOnly), r.lastEmbeddingInput())
	r.expect(res.hit() == "false" && after.Chat == before.Chat+1,
		"a paraphrase (mock cosine %.4f, above the %v threshold) misses: GetNearest only considers rows with an embedding", wouldBe, r.threshold)
	r.note("NULL-embedding row %s; direct hit ok; paraphrase with cosine %.4f misses", id, wouldBe)
	r.end()
	return nil
}

func (r *runner) stepX2TTL() error {
	r.begin("x2", "x-bf-cache-ttl: 2s -> entry expires; the expired row is deleted and rewritten", false)
	res := r.chat(qTTL, chatOpts{headers: h("x-bf-cache-key", keyDemo, "x-bf-cache-ttl", "2s")})
	id := res.cacheID()
	r.expect(res.hit() == "false" && r.waitRowState(r.namespace, id, true, 10*time.Second), "miss, row %s written", id)
	expIn := atoiOr(r.rowField(r.namespace, id, "(metadata->>'expires_at')::bigint - extract(epoch from now())::bigint"), -999)
	r.expect(expIn >= 0 && expIn <= 3, "expires_at is about 2 s ahead (expires_in = %d s)", expIn)
	res = r.chat(qTTL, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	r.expect(res.hit() == "true", "hit before expiry")
	r.printf("  waiting 3.5 s for the entry to expire\n")
	time.Sleep(3500 * time.Millisecond)
	before := r.stats()
	res = r.chat(qTTL, chatOpts{headers: h("x-bf-cache-key", keyDemo)})
	after := r.stats()
	r.expect(res.hit() == "false" && after.Chat == before.Chat+1, "after expiry: miss and one upstream call")
	time.Sleep(1500 * time.Millisecond)
	exists := r.rowExists(r.namespace, id)
	expIn2 := atoiOr(r.rowField(r.namespace, id, "(metadata->>'expires_at')::bigint - extract(epoch from now())::bigint"), -999)
	r.expect(exists && expIn2 > 1000, "row %s rewritten with the default TTL after the async delete of the expired one (exists=%t, expires_in=%d s)", id, exists, expIn2)
	r.note("expires_at stored as a JSON number, read back as float64 by the plugin; expired -> miss -> rewritten (expires_in %d s)", expIn2)
	r.end()
	return nil
}

func (r *runner) stepX3Stream() error {
	r.begin("x3", "streaming: chunks stored as a JSONB string array and replayed on a hit", false)
	before := r.stats()
	res := r.chat(qStream, chatOpts{stream: true, headers: h("x-bf-cache-key", keyDemo)})
	after := r.stats()
	id := res.cacheID()
	r.expect(res.Status == 200 && res.Content == "Mock answer to: "+qStream, "stream content %q", res.Content)
	r.expect(res.hit() == "false" && after.Stream == before.Stream+1, "miss; one upstream streaming call")
	r.expect(r.waitRowState(r.namespace, id, true, 10*time.Second), "row %s written after the final chunk", id)
	chunks := atoiOr(r.rowField(r.namespace, id, "jsonb_array_length(metadata->'stream_chunks')"), 0)
	hasResp := r.rowField(r.namespace, id, "metadata ? 'response'")
	r.expect(chunks > 0, "row stores %d stream chunks (response field present: %s)", chunks, hasResp)
	r.showRows(r.namespace, fmt.Sprintf(" WHERE id = '%s'", id))
	before = r.stats()
	res2 := r.chat(qStream, chatOpts{stream: true, headers: h("x-bf-cache-key", keyDemo)})
	after = r.stats()
	r.expect(res2.hit() == "true" && res2.Cache["x-bifrost-cache-hit-type"] == "direct" && after.Chat == before.Chat, "second identical stream: direct hit from headers, no upstream call")
	r.expect(res2.Content == res.Content && res2.ID == res.ID, "replayed content and id identical (%s)", res2.ID)
	hit, _ := res2.CacheDebug["cache_hit"].(bool)
	r.expect(hit, "the final chunk carries extra_fields.cache_debug with cache_hit true")
	r.note("%d chunks stored; replay identical; cache_debug on the final chunk", chunks)
	r.end()
	return nil
}

func (r *runner) stepX4OpenAIRoute() error {
	r.begin("x4", "OpenAI SDK route /openai/v1/chat/completions: cache headers and body", false)
	res := r.chat(qBase, chatOpts{path: "/openai/v1/chat/completions", headers: h("x-bf-cache-key", keyDemo)})
	r.expect(res.Status == 200, "HTTP 200 (got %d)", res.Status)
	r.expect(res.hit() != "", "x-bifrost-cache-* headers present (hit=%s type=%s)", res.hit(), res.Cache["x-bifrost-cache-hit-type"])
	_, hasExtra := res.JSON["extra_fields"]
	r.printf("  body has extra_fields: %t; top-level keys: %s\n", hasExtra, keysOf(res.JSON))
	r.note("hit=%s type=%s id=%s; body extra_fields present=%t", res.hit(), res.Cache["x-bifrost-cache-hit-type"], res.cacheID(), hasExtra)
	r.end()
	return nil
}

// stepX6QueryPlan shows which plan PostgreSQL uses for the semantic lookup the plugin sends
// (same predicates as PgVectorStore.GetNearest), and that the HNSW index path, when forced,
// returns the same row.
func (r *runner) stepX6QueryPlan() error {
	r.begin("x6", "query plan of the semantic lookup; forced HNSW index scan returns the same row", false)
	if !isUUID(r.idA) {
		r.expect(false, "no entry from (a)")
		r.end()
		return nil
	}
	vec := r.rowField(r.namespace, r.idA, "embedding::text")
	paramsHash := r.rowField(r.namespace, r.idA, "metadata->>'params_hash'")
	if !r.expect(strings.HasPrefix(vec, "[") && identPattern.MatchString("h"+paramsHash), "query vector = stored embedding of %s, params_hash %s", r.idA, paramsHash) {
		r.end()
		return nil
	}
	lit := "'" + vec + "'::vector"
	query := fmt.Sprintf(`SELECT id, 1 - (embedding <=> %[1]s) AS score FROM %[2]s WHERE embedding IS NOT NULL AND (embedding <=> %[1]s) <= %[3]v::float8 AND metadata @> '{"cache_key":"%[4]s"}'::jsonb AND metadata @> '{"params_hash":"%[5]s"}'::jsonb AND metadata @> '{"from_bifrost_semantic_cache_plugin":true}'::jsonb AND metadata @> '{"provider":"openai"}'::jsonb AND metadata @> '{"model":"mock-chat"}'::jsonb ORDER BY embedding <=> %[1]s LIMIT 1`,
		lit, r.table(r.namespace), 1-r.threshold, keyDemo, paramsHash)
	rows := r.rowCount(r.namespace)
	plan := func(prefix string) string {
		out, err := r.psqlRaw(r.opt.pgDB, prefix+"SET LOCAL hnsw.iterative_scan = strict_order; EXPLAIN (ANALYZE, COSTS OFF, SUMMARY OFF) "+query, "-At")
		if err != nil {
			return "ERROR " + err.Error() + " " + out
		}
		return out
	}
	shorten := func(p string) string {
		var keep []string
		for _, line := range strings.Split(p, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "SET" {
				continue
			}
			line = vectorLiteralRe.ReplaceAllString(line, "'[...]'")
			keep = append(keep, line)
		}
		return strings.Join(keep, "\n        ")
	}
	def := plan("")
	r.printf("  default plan with %d rows in the table (vector literal elided):\n        %s\n", rows, shorten(def))
	usesSeq := strings.Contains(def, "Seq Scan")
	usesHNSW := strings.Contains(def, "_emb_hnsw")
	r.note("default plan at %d rows: seq scan=%t, hnsw index=%t", rows, usesSeq, usesHNSW)
	forced := plan("SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off; ")
	r.printf("  plan with enable_seqscan=off, enable_bitmapscan=off:\n        %s\n", shorten(forced))
	r.expect(strings.Contains(forced, "Index Scan using") && strings.Contains(forced, "_emb_hnsw"), "the HNSW index can serve the query (Index Scan using ..._emb_hnsw)")
	idDefault, _ := r.psqlRaw(r.opt.pgDB, "SET LOCAL hnsw.iterative_scan = strict_order; SELECT id FROM ("+query+") q", "-At")
	idForced, _ := r.psqlRaw(r.opt.pgDB, "SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off; SET LOCAL hnsw.iterative_scan = strict_order; SELECT id FROM ("+query+") q", "-At")
	idDefault, idForced = lastLine(idDefault), lastLine(idForced) // psql echoes a SET tag per SET LOCAL
	r.expect(idDefault != "" && idDefault == idForced, "both plans return the same nearest row (%s / %s)", idDefault, idForced)
	r.note("forced HNSW plan returns the same row %s", idForced)
	r.end()
	return nil
}

var vectorLiteralRe = regexp.MustCompile(`'\[[^\]]*\]'`)

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func keysOf(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// stepX5AuthProbe starts a second gateway with a Postgres config store (in a
// scratch database) and a setup token, to show what /api/cache/* requires then.
func (r *runner) stepX5AuthProbe() error {
	r.begin("x5", "auth on /api/cache/* when a config store IS enabled (OSS setup-token lock)", false)
	cfgPath := filepath.Join(r.opt.authProbeDir, "config.json")
	data, err := os.ReadFile(cfgPath)
	if !r.expect(err == nil, "read %s", cfgPath) {
		r.end()
		return nil
	}
	var probeCfg struct {
		SetupToken  string `json:"setup_token"`
		ConfigStore struct {
			Config struct {
				DBName string `json:"db_name"`
			} `json:"config"`
		} `json:"config_store"`
		Governance struct {
			VirtualKeys []struct {
				Value string `json:"value"`
			} `json:"virtual_keys"`
		} `json:"governance"`
		Plugins []struct {
			Name   string         `json:"name"`
			Config map[string]any `json:"config"`
		} `json:"plugins"`
	}
	_ = json.Unmarshal(data, &probeCfg)
	scratchDB := probeCfg.ConfigStore.Config.DBName
	vk := ""
	if len(probeCfg.Governance.VirtualKeys) > 0 {
		vk = probeCfg.Governance.VirtualKeys[0].Value
	}
	probeNS := ""
	for _, p := range probeCfg.Plugins {
		if p.Name == "semantic_cache" {
			probeNS, _ = p.Config["vector_store_namespace"].(string)
		}
	}
	if !r.expect(identPattern.MatchString(scratchDB) && identPattern.MatchString(probeNS) && probeCfg.SetupToken != "" && strings.HasPrefix(vk, "sk-bf-"),
		"probe config: scratch config-store db %q, namespace %q, setup token set, virtual key %q", scratchDB, probeNS, vk) {
		r.end()
		return nil
	}
	probeAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.opt.authProbePort))
	if !r.expect(!portInUse(probeAddr), "port %s is free", probeAddr) {
		r.end()
		return nil
	}
	for _, sql := range []string{
		"DROP DATABASE IF EXISTS " + qident(scratchDB) + " WITH (FORCE)",
		"CREATE DATABASE " + qident(scratchDB),
		"DROP TABLE IF EXISTS " + r.table(probeNS),
	} {
		out, err := r.psqlRaw(r.opt.pgDB, sql)
		r.printf("  psql> %s\n        %s\n", sql, strings.ReplaceAll(out, "\n", "\n        "))
		if err != nil {
			r.expect(false, "psql failed: %v", err)
			r.end()
			return nil
		}
	}
	appDir, _ := filepath.Abs(r.opt.authProbeDir)
	args := []string{"-app-dir", appDir, "-port", strconv.Itoa(r.opt.authProbePort), "-host", "127.0.0.1", "-log-level", "info", "-log-style", "pretty"}
	logPath := filepath.Join(r.opt.procLogDir, "gateway-authprobe.log")
	p, err := startProc("bifrost-http (auth probe)", r.opt.gatewayBin, args, appDir, logPath)
	if !r.expect(err == nil, "start auth-probe gateway: %v", err) {
		r.end()
		return nil
	}
	r.probe = p
	r.pids = append(r.pids, fmt.Sprintf("bifrost-http auth probe pid %d on %s", p.pid(), probeAddr))
	r.printf("  started auth-probe gateway pid=%d (config store: postgres db %s; setup_token set) log: %s\n", p.pid(), scratchDB, logPath)
	probeURL := "http://" + probeAddr
	defer func() {
		r.probe.stop()
		for _, sql := range []string{"DROP TABLE IF EXISTS " + r.table(probeNS), "DROP DATABASE IF EXISTS " + qident(scratchDB) + " WITH (FORCE)"} {
			out, err := r.psqlRaw(r.opt.pgDB, sql)
			r.printf("  cleanup psql> %s -> %s %s\n", sql, strings.ReplaceAll(out, "\n", " | "), errString(err))
		}
	}()
	if !r.expect(r.waitHealthy(p, probeURL+"/health", 180*time.Second) == nil, "auth-probe gateway healthy") {
		r.end()
		return nil
	}
	for _, line := range gatewayLogLines(logPath, "/api surface", "setup token", "Setup-Token", "admin account", "plugin status: semantic_cache") {
		r.printf("  gateway log: %s\n", line)
	}
	// enforce_auth_on_inference is omitted in the probe config: on a fresh config-store
	// deployment it resolves to true, so inference needs a virtual key.
	noVK := r.chat(qBase, chatOpts{base: probeURL, headers: h("x-bf-cache-key", "authprobe")})
	r.expect(noVK.Status == 401 && strings.Contains(string(noVK.Body), "virtual_key_required") && len(noVK.Cache) == 0,
		"inference without x-bf-vk -> 401 virtual_key_required, no cache lookup (got %d)", noVK.Status)
	res := r.chat(qBase, chatOpts{base: probeURL, headers: h("x-bf-cache-key", "authprobe", "x-bf-vk", vk)})
	id := res.cacheID()
	if !r.expect(res.Status == 200 && res.hit() == "false" && isUUID(id), "inference with x-bf-vk -> 200, cache miss, cache id %s (HTTP %d)", id, res.Status) {
		r.end()
		return nil
	}
	r.expect(r.waitRowState(probeNS, id, true, 10*time.Second), "entry written to %s", r.table(probeNS))
	again := r.chat(qBase, chatOpts{base: probeURL, headers: h("x-bf-cache-key", "authprobe", "x-bf-vk", vk)})
	r.expect(again.hit() == "true" && again.cacheID() == id, "same request with the same virtual key -> direct hit")
	url := probeURL + "/api/cache/clear/" + id
	noAuth := r.api("DELETE", url, nil)
	r.expect(noAuth.Status == 401, "no credential -> 401 (got %d)", noAuth.Status)
	wrong := r.api("DELETE", url, h("X-Bifrost-Setup-Token", "wrong-token"))
	r.expect(wrong.Status == 403, "wrong X-Bifrost-Setup-Token -> 403 (got %d)", wrong.Status)
	r.expect(r.rowExists(probeNS, id), "entry still present after the refused calls")
	right := r.api("DELETE", url, h("X-Bifrost-Setup-Token", probeCfg.SetupToken))
	r.expect(right.Status == 200, "correct X-Bifrost-Setup-Token -> 200 (got %d)", right.Status)
	r.expect(!r.waitRowState(probeNS, id, false, 5*time.Second), "entry deleted")
	byKey := r.api("DELETE", probeURL+"/api/cache/clear-by-key/authprobe", h("X-Bifrost-Setup-Token", probeCfg.SetupToken))
	r.expect(byKey.Status == 200, "clear-by-key with the setup token -> 200 (got %d)", byKey.Status)
	r.note("config store on: inference without x-bf-vk -> %d; /api/cache/clear without credential -> %d, wrong setup token -> %d, X-Bifrost-Setup-Token -> %d (until dashboard auth is enabled; then Basic/Bearer session auth applies)", noVK.Status, noAuth.Status, wrong.Status, right.Status)
	r.end()
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
