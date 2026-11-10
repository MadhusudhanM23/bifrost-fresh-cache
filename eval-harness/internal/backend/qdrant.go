package backend

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"
	"github.com/qdrant/go-client/qdrant"
)

func newQdrant(ctx context.Context, opts Options, logger schemas.Logger) (*Backend, error) {
	store, err := newStore(ctx, vectorstore.VectorStoreTypeQdrant, vectorstore.QdrantConfig{
		Host: secret(opts.QdrantHost),
		Port: secret(itoa(opts.QdrantPort)),
	}, logger)
	if err != nil {
		return nil, err
	}
	client, err := qdrant.NewClient(&qdrant.Config{Host: opts.QdrantHost, Port: opts.QdrantPort, SkipCompatibilityCheck: true})
	if err != nil {
		_ = store.Close(ctx, "")
		return nil, fmt.Errorf("qdrant side channel: %w", err)
	}

	b := &Backend{Name: "qdrant", Store: store}
	b.Server = func(ctx context.Context) map[string]any {
		reply, err := client.HealthCheck(ctx)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		return map[string]any{"title": reply.GetTitle(), "version": reply.GetVersion(), "commit": reply.GetCommit()}
	}
	b.Ready = func(ctx context.Context, ns string) (string, error) {
		// Qdrant acknowledges an upsert (wait=true) once it is durable, then builds
		// the HNSW index in the background. Querying while the optimizer runs would
		// measure a moving target, so wait until the collection reports green with
		// the optimizer idle on several consecutive polls.
		start := time.Now()
		deadline := start.Add(20 * time.Minute)
		consecutive := 0
		for {
			info, err := client.GetCollectionInfo(ctx, ns)
			if err != nil {
				return "", err
			}
			if info.GetStatus() == qdrant.CollectionStatus_Green && info.GetOptimizerStatus().GetOk() {
				consecutive++
			} else {
				consecutive = 0
			}
			if consecutive >= 3 {
				return fmt.Sprintf("waited %.1fs until the collection was green with the optimizer idle on 3 consecutive polls; indexed_vectors_count=%d of points_count=%d, segments=%d",
					time.Since(start).Seconds(), info.GetIndexedVectorsCount(), info.GetPointsCount(), info.GetSegmentsCount()), nil
			}
			if time.Now().After(deadline) {
				return "", fmt.Errorf("collection %s not green after %s (status %s)", ns, time.Since(start).Round(time.Second), info.GetStatus())
			}
			time.Sleep(time.Second)
		}
	}
	b.Describe = func(ctx context.Context, ns string) map[string]any {
		info, err := client.GetCollectionInfo(ctx, ns)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		out := map[string]any{
			"status":                info.GetStatus().String(),
			"points_count":          info.GetPointsCount(),
			"indexed_vectors_count": info.GetIndexedVectorsCount(),
			"segments_count":        info.GetSegmentsCount(),
		}
		cfg := info.GetConfig()
		if params := cfg.GetParams().GetVectorsConfig().GetParams(); params != nil {
			out["vector_size"] = params.GetSize()
			out["distance"] = params.GetDistance().String()
		}
		if h := cfg.GetHnswConfig(); h != nil {
			out["hnsw"] = map[string]any{"m": h.GetM(), "ef_construct": h.GetEfConstruct(), "full_scan_threshold_kb": h.GetFullScanThreshold(), "on_disk": h.GetOnDisk()}
		}
		if o := cfg.GetOptimizerConfig(); o != nil {
			out["optimizer"] = map[string]any{"indexing_threshold_kb": o.GetIndexingThreshold(), "default_segment_number": o.GetDefaultSegmentNumber(), "max_segment_size_kb": o.GetMaxSegmentSize()}
		}
		schema := map[string]any{}
		names := make([]string, 0, len(info.GetPayloadSchema()))
		for name := range info.GetPayloadSchema() {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			s := info.GetPayloadSchema()[name]
			schema[name] = map[string]any{"type": s.GetDataType().String(), "points": s.GetPoints()}
		}
		out["payload_indexes"] = schema
		return out
	}
	b.Close = func() {
		_ = store.Close(context.Background(), "")
		_ = client.Close()
	}
	return b, nil
}
