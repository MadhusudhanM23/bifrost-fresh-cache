package backend

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"
	"github.com/redis/go-redis/v9"
)

// newRedis builds the Redis (RediSearch) store. Optional: it needs Redis Stack.
func newRedis(ctx context.Context, opts Options, logger schemas.Logger) (*Backend, error) {
	store, err := newStore(ctx, vectorstore.VectorStoreTypeRedis, vectorstore.RedisConfig{Addr: schemas.NewSecretVar(opts.RedisAddr)}, logger)
	if err != nil {
		return nil, err
	}
	// RESP2 keeps FT.INFO a flat key/value list.
	client := redis.NewClient(&redis.Options{Addr: opts.RedisAddr, Protocol: 2})
	ftInfo := func(ctx context.Context, ns string) (map[string]string, error) {
		raw, err := client.Do(ctx, "FT.INFO", ns).Slice()
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		for i := 0; i+1 < len(raw); i += 2 {
			key := fmt.Sprint(raw[i])
			switch v := raw[i+1].(type) {
			case string, int64, float64:
				out[key] = fmt.Sprint(v)
			}
		}
		return out, nil
	}

	b := &Backend{Name: "redis", Store: store}
	b.Server = func(ctx context.Context) map[string]any {
		out := map[string]any{}
		if info, err := client.Info(ctx, "server").Result(); err == nil {
			for _, line := range strings.Split(info, "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "redis_version:"); ok {
					out["redis_version"] = v
				}
			}
		} else {
			out["error"] = err.Error()
		}
		if modules, err := client.Do(ctx, "MODULE", "LIST").Slice(); err == nil {
			var names []string
			for _, m := range modules {
				if fields, ok := m.([]interface{}); ok {
					var name, ver string
					for i := 0; i+1 < len(fields); i += 2 {
						switch fmt.Sprint(fields[i]) {
						case "name":
							name = fmt.Sprint(fields[i+1])
						case "ver":
							ver = fmt.Sprint(fields[i+1])
						}
					}
					names = append(names, name+"@"+ver)
				}
			}
			out["modules"] = names
		}
		return out
	}
	b.Ready = func(ctx context.Context, ns string) (string, error) {
		start := time.Now()
		for {
			info, err := ftInfo(ctx, ns)
			if err != nil {
				return "", err
			}
			if info["indexing"] == "0" {
				return fmt.Sprintf("FT.INFO reported indexing=0 after %.1fs (num_docs=%s)", time.Since(start).Seconds(), info["num_docs"]), nil
			}
			if time.Since(start) > 10*time.Minute {
				return "", fmt.Errorf("index %s still indexing after 10 minutes", ns)
			}
			time.Sleep(time.Second)
		}
	}
	b.Describe = func(ctx context.Context, ns string) map[string]any {
		info, err := ftInfo(ctx, ns)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		out := map[string]any{"index_algorithm": "HNSW (FT.CREATE in redis.go, COSINE, FLOAT32)"}
		for _, key := range []string{"num_docs", "num_records", "indexing", "percent_indexed", "hash_indexing_failures", "vector_index_sz_mb", "total_index_memory_sz_mb"} {
			if v, ok := info[key]; ok {
				out[key] = v
			}
		}
		return out
	}
	b.Close = func() {
		_ = store.Close(context.Background(), "")
		_ = client.Close()
	}
	return b, nil
}
