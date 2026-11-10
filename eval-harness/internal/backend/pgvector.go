package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"
)

// pgSettings are recorded with every pgvector result. synchronous_commit and
// fsync matter for write latency (every Add commits on its own); the hnsw.*
// settings shape GetNearest.
var pgSettings = []string{
	"shared_buffers", "work_mem", "maintenance_work_mem", "effective_cache_size",
	"synchronous_commit", "fsync", "wal_sync_method", "full_page_writes", "wal_level",
	"max_connections", "max_parallel_workers_per_gather", "jit", "random_page_cost", "plan_cache_mode",
	"hnsw.ef_search", "hnsw.iterative_scan", "hnsw.max_scan_tuples", "hnsw.scan_mem_multiplier",
}

const pgSchema = "public" // the store's default schema; the harness does not change it

func newPgVector(ctx context.Context, opts Options, logger schemas.Logger) (*Backend, error) {
	cfg := vectorstore.PgVectorConfig{
		Host:     secret(opts.PgHost),
		Port:     secret(itoa(opts.PgPort)),
		User:     secret(opts.PgUser),
		Password: secret(opts.PgPassword),
		Database: secret(opts.PgDatabase),
		SSLMode:  secret(opts.PgSSLMode),
	}
	if opts.PgMaxConns > 0 {
		cfg.MaxConns = secret(itoa(opts.PgMaxConns))
	}
	store, err := newStore(ctx, vectorstore.VectorStoreTypePgVector, cfg, logger)
	if err != nil {
		return nil, err
	}

	connURL := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(opts.PgUser, opts.PgPassword),
		Host:     net.JoinHostPort(opts.PgHost, strconv.Itoa(opts.PgPort)),
		Path:     "/" + opts.PgDatabase,
		RawQuery: url.Values{"sslmode": {opts.PgSSLMode}}.Encode(),
	}
	pool, err := pgxpool.New(ctx, connURL.String())
	if err != nil {
		_ = store.Close(ctx, "")
		return nil, fmt.Errorf("pgvector side channel: %w", err)
	}
	table := func(ns string) string { return pgx.Identifier{pgSchema, ns}.Sanitize() }

	b := &Backend{Name: "pgvector", Store: store}
	b.Server = func(ctx context.Context) map[string]any {
		out := map[string]any{}
		conn, err := pool.Acquire(ctx)
		if err != nil {
			out["error"] = err.Error()
			return out
		}
		defer conn.Release()
		// Using a vector loads the pgvector library into this session, which is
		// what makes its hnsw.* settings visible to current_setting.
		_, _ = conn.Exec(ctx, `SELECT '[1,0]'::vector <=> '[0,1]'::vector`)
		scalar := func(key, sql string, args ...any) {
			var v *string
			if err := conn.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
				out[key] = "error: " + err.Error()
			} else if v == nil {
				out[key] = nil
			} else {
				out[key] = *v
			}
		}
		scalar("version", `SELECT version()`)
		scalar("server_version", `SHOW server_version`)
		scalar("pgvector_version", `SELECT extversion FROM pg_extension WHERE extname = 'vector'`)
		// The database may be shared; other work running alongside would show up here.
		scalar("other_sessions_at_start", `SELECT format('%s other client sessions, %s of them active', count(*), count(*) FILTER (WHERE state = 'active'))
			FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()`)
		settings := map[string]any{}
		for _, name := range pgSettings {
			var v *string
			if err := conn.QueryRow(ctx, `SELECT current_setting($1, true)`, name).Scan(&v); err != nil {
				settings[name] = "error: " + err.Error()
			} else if v == nil {
				settings[name] = nil
			} else {
				settings[name] = *v
			}
		}
		out["settings"] = settings
		return out
	}
	b.Ready = func(ctx context.Context, ns string) (string, error) {
		// A freshly bulk-loaded table has no planner statistics until autovacuum
		// gets to it, and the plan GetNearest gets can change when it does. Doing
		// what autovacuum would do, once and up front, keeps the plan stable for
		// the whole query phase.
		start := time.Now()
		if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) `+table(ns)); err != nil {
			return "", fmt.Errorf("VACUUM (ANALYZE): %w", err)
		}
		return fmt.Sprintf("ran VACUUM (ANALYZE) on %s in %.1fs (what autovacuum would eventually do), so planner statistics exist and stay fixed during the query phase", table(ns), time.Since(start).Seconds()), nil
	}
	b.Describe = func(ctx context.Context, ns string) map[string]any {
		out := map[string]any{"table": table(ns)}
		var rows, tableBytes, indexBytes, totalBytes, toastBytes int64
		var storage string
		err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT (SELECT count(*) FROM %s), pg_table_size(c.oid), pg_indexes_size(c.oid), pg_total_relation_size(c.oid),
			COALESCE(pg_relation_size(NULLIF(c.reltoastrelid, 0)), 0),
			(SELECT attstorage::text FROM pg_attribute WHERE attrelid = c.oid AND attname = 'embedding')
			FROM pg_class c WHERE c.oid = to_regclass($1)`, table(ns)), table(ns)).Scan(&rows, &tableBytes, &indexBytes, &totalBytes, &toastBytes, &storage)
		if err != nil {
			out["error"] = err.Error()
			return out
		}
		out["rows"] = rows
		out["table_bytes"] = tableBytes
		out["toast_bytes"] = toastBytes
		out["indexes_bytes"] = indexBytes
		out["total_bytes"] = totalBytes
		out["embedding_attstorage"] = storage
		indexRows, err := pool.Query(ctx, `SELECT c.relname, pg_get_indexdef(c.oid), pg_relation_size(c.oid), COALESCE(array_to_string(c.reloptions, ','), '')
			FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = to_regclass($1) ORDER BY c.relname`, table(ns))
		if err != nil {
			out["indexes_error"] = err.Error()
			return out
		}
		defer indexRows.Close()
		var indexes []map[string]any
		for indexRows.Next() {
			var name, def, options string
			var size int64
			if err := indexRows.Scan(&name, &def, &size, &options); err != nil {
				out["indexes_error"] = err.Error()
				break
			}
			if options == "" {
				options = "none"
				if strings.Contains(def, "USING hnsw") {
					options = "none, so pgvector's defaults m=16, ef_construction=64"
				}
			}
			indexes = append(indexes, map[string]any{"name": name, "definition": def, "bytes": size, "reloptions": options})
		}
		out["indexes"] = indexes
		return out
	}
	b.Explain = func(ctx context.Context, ns string, samples map[string]SampleQuery) map[string]string {
		out := map[string]string{}
		for name, sample := range samples {
			sql, args, err := pgNearestSQL(table(ns), sample)
			if err != nil {
				out[name] = "error: " + err.Error()
				continue
			}
			out[name+": EXPLAIN ANALYZE (custom plan for these values)"] = abbreviateVectors(explainAnalyze(ctx, pool, sql, args))
			out[name+": EXPLAIN (GENERIC_PLAN)"] = abbreviateVectors(explainGeneric(ctx, pool, sql))
		}
		return out
	}
	b.Close = func() {
		_ = store.Close(context.Background(), "")
		pool.Close()
	}
	return b, nil
}

// pgNearestSQL reproduces the statement PgVectorStore.GetNearest sends
// (bifrost/framework/vectorstore/pgvector.go), for EXPLAIN only: same WHERE,
// ORDER BY and LIMIT, same placeholders and filter casts. The store binds the
// vector in pgvector's binary format ($1::vector) when it could register the
// type and as text ($1::text::vector) otherwise; this copy always binds text,
// which does not change the plan (the cast runs once on a parameter). Only
// Equal filters with values are reproduced, which is all the semantic cache
// sends. If pgvector.go changes, this copy must follow it; the benchmark itself
// always goes through the store.
func pgNearestSQL(table string, sample SampleQuery) (string, []any, error) {
	args := []any{vectorText(sample.Vector), 1 - sample.Threshold}
	conditions := []string{"embedding IS NOT NULL", "(embedding <=> $1::text::vector) <= $2::float8"}
	for _, q := range sample.Filters {
		if q.Operator != vectorstore.QueryOperatorEqual || q.Value == nil {
			return "", nil, fmt.Errorf("only Equal filters with a value are reproduced")
		}
		doc, err := json.Marshal(map[string]any{q.Field: q.Value})
		if err != nil {
			return "", nil, err
		}
		args = append(args, string(doc))
		conditions = append(conditions, fmt.Sprintf("metadata @> $%d::text::jsonb", len(args)))
	}
	limit := sample.Limit
	if limit <= 0 {
		limit = 10
	}
	args = append(args, limit)
	sql := fmt.Sprintf(`SELECT id, metadata, 1 - (embedding <=> $1::text::vector) AS score FROM %s WHERE %s ORDER BY embedding <=> $1::text::vector LIMIT $%d`,
		table, strings.Join(conditions, " AND "), len(args))
	return sql, args, nil
}

var vectorLiteralPattern = regexp.MustCompile(`'\[[-0-9.e,]+\]'`)

// abbreviateVectors shortens the vector literals PostgreSQL prints in a plan
// (17 KB each at 1536 dimensions) to their length.
func abbreviateVectors(plan string) string {
	return vectorLiteralPattern.ReplaceAllStringFunc(plan, func(literal string) string {
		return fmt.Sprintf("'[%d floats]'", strings.Count(literal, ",")+1)
	})
}

func vectorText(v []float32) string {
	buf := make([]byte, 0, len(v)*10+2)
	buf = append(buf, '[')
	for i, x := range v {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = strconv.AppendFloat(buf, float64(x), 'g', -1, 32)
	}
	return string(append(buf, ']'))
}

// explainAnalyze runs the statement under EXPLAIN ANALYZE with the same
// session setting the store uses (iterative index scans, pgvector >= 0.8).
func explainAnalyze(ctx context.Context, pool *pgxpool.Pool, sql string, args []any) string {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "error: " + err.Error()
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = strict_order`); err != nil {
		return "error: " + err.Error()
	}
	rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS) `+sql, args...)
	if err != nil {
		return "error: " + err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "error: " + err.Error()
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return "error: " + err.Error()
	}
	return strings.Join(lines, "\n")
}

// explainGeneric shows the parameter-independent plan PostgreSQL may switch to
// for a prepared statement after five executions (plan_cache_mode = auto). The
// store's statements are prepared by pgx, so this is the other plan they can get.
func explainGeneric(ctx context.Context, pool *pgxpool.Pool, sql string) string {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return "error: " + err.Error()
	}
	defer conn.Release()
	results, err := conn.Conn().PgConn().Exec(ctx, `BEGIN; SET LOCAL hnsw.iterative_scan = strict_order; EXPLAIN (GENERIC_PLAN) `+sql+`; ROLLBACK`).ReadAll()
	if err != nil {
		return "error: " + err.Error()
	}
	var lines []string
	for _, r := range results {
		for _, row := range r.Rows {
			if len(row) > 0 {
				lines = append(lines, string(row[0]))
			}
		}
	}
	return strings.Join(lines, "\n")
}
