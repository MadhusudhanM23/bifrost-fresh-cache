package main

import (
	"context"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"

	"bifrost-eval-harness/internal/backend"
	"bifrost-eval-harness/internal/clock"
	"bifrost-eval-harness/internal/cpuload"
	"bifrost-eval-harness/internal/stats"
	"bifrost-eval-harness/internal/workload"
)

// bench is one backend's state while a dimension is measured.
type bench struct {
	res     *BackendResult
	b       *backend.Backend
	store   vectorstore.VectorStore
	ns      string
	failed  bool
	nextAdd int
}

func (st *bench) fail(stage string, err error) {
	st.res.Failure = stage + ": " + err.Error()
	st.failed = true
	logf("[%s D=%d] FAILED %s", st.res.Backend, st.res.Dim, st.res.Failure)
}

// runDimension measures every backend at one dimension. Backends are
// interleaved: for each run, op and concurrency the backends' phases run back
// to back, so a burst of outside load lands on both backends' measurements
// instead of on whichever happened to be running.
func runDimension(ctx context.Context, ds *workload.Dataset, sets map[string]*querySet, cfg benchConfig, opts backend.Options, logger schemas.Logger, report *Report, save func()) {
	var benches []*bench
	for _, name := range cfg.Backends {
		st := setupBackend(ctx, name, ds, cfg, opts, logger)
		benches = append(benches, st)
		report.Results = append(report.Results, st.res)
		save()
	}
	active := func(yield func(*bench) bool) {
		for _, st := range benches {
			if !st.failed && !yield(st) {
				return
			}
		}
	}

	for run := 1; run <= cfg.Runs; run++ {
		for _, c := range cfg.Concurrency {
			for st := range active {
				measureAdds(ctx, st, ds, cfg, run, c)
			}
		}
	}
	for st := range active {
		prepareQueries(ctx, st, ds, sets, cfg)
	}
	save()
	for run := 1; run <= cfg.Runs; run++ {
		for _, op := range cfg.Ops {
			for _, c := range cfg.Concurrency {
				for st := range active {
					measureQueries(ctx, st, ds, sets[op], cfg, op, run, c)
				}
			}
		}
		save()
	}
	for _, st := range benches {
		finishBackend(st, cfg)
	}
	save()
}

// setupBackend connects, creates the namespace and preloads it.
func setupBackend(ctx context.Context, name string, ds *workload.Dataset, cfg benchConfig, opts backend.Options, logger schemas.Logger) *bench {
	st := &bench{res: &BackendResult{
		Backend: name, Dim: ds.Dim, Entries: len(ds.Entries), Keys: len(ds.Partitions),
		PartitionSize: ds.PartitionSize(0), Correctness: map[string]*Tally{},
		Namespace: fmt.Sprintf("%s_%s_d%d", cfg.NamespacePrefix, name, ds.Dim),
	}, ns: fmt.Sprintf("%s_%s_d%d", cfg.NamespacePrefix, name, ds.Dim)}
	logf("[%s D=%d] connecting", name, ds.Dim)
	b, err := backend.New(ctx, name, opts, logger)
	if err != nil {
		st.fail("connect", err)
		return st
	}
	st.b, st.store = b, b.Store
	st.res.Server = b.Server(ctx)

	// A namespace left by an interrupted run of this harness (same name) is removed first.
	if err := st.store.DeleteNamespace(ctx, st.ns); err != nil {
		st.fail("remove leftover namespace", err)
		return st
	}
	t0 := clock.Now()
	if err := st.store.CreateNamespace(ctx, st.ns, ds.Dim, workload.CacheProperties); err != nil {
		st.fail("CreateNamespace", err)
		return st
	}
	st.res.CreateNamespaceMs = float64(clock.Since(t0)) / float64(time.Millisecond)

	preload := len(ds.Entries) - cfg.WriteSample*len(cfg.Concurrency)*cfg.Runs
	logf("[%s D=%d] preloading %d entries at concurrency %d", name, ds.Dim, preload, cfg.LoadConcurrency)
	s0 := cpuload.Read()
	pr := stats.RunPhase(preload, cfg.LoadConcurrency, func(i int) error { return addEntry(ctx, st, ds, i) })
	st.res.Preload = &PhaseRecord{Op: "preload", Concurrency: cfg.LoadConcurrency, Summary: pr.Summary(), FirstError: errText(pr.FirstErr),
		CPUBusyPct: cpuload.BusyPercent(s0, cpuload.Read()),
		Note:       fmt.Sprintf("entries 0..%d into an empty namespace; not part of the medians", preload)}
	logf("[%s D=%d] preload: %.0f ops/s, p50 %.2f ms, errors %d", name, ds.Dim, pr.Summary().Throughput, pr.Summary().P50Ms, pr.Errors)
	if pr.Errors == preload && preload > 0 {
		st.fail("preload", pr.FirstErr)
	}
	st.nextAdd = preload
	return st
}

func addEntry(ctx context.Context, st *bench, ds *workload.Dataset, i int) error {
	e := ds.Entries[i]
	return st.store.Add(ctx, st.ns, e.ID, e.Vector, e.Metadata)
}

// measureAdds adds the next slice of entries at concurrency c.
func measureAdds(ctx context.Context, st *bench, ds *workload.Dataset, cfg benchConfig, run, c int) {
	from := st.nextAdd
	st.nextAdd += cfg.WriteSample
	s0 := cpuload.Read()
	pr := stats.RunPhase(cfg.WriteSample, c, func(i int) error { return addEntry(ctx, st, ds, from+i) })
	rec := PhaseRecord{Op: opAdd, Concurrency: c, Run: run, Summary: pr.Summary(), FirstError: errText(pr.FirstErr),
		CPUBusyPct: cpuload.BusyPercent(s0, cpuload.Read()),
		Note:       fmt.Sprintf("namespace held %d entries at the start of the phase", from)}
	st.res.Phases = append(st.res.Phases, rec)
	logf("[%s D=%d] run %d add c=%d: p50 %.2f p99 %.2f ms, %.0f ops/s, errors %d%s", st.res.Backend, ds.Dim, run, c,
		rec.Summary.P50Ms, rec.Summary.P99Ms, rec.Summary.Throughput, rec.Summary.Errors, cpuText(rec.CPUBusyPct))
}

// prepareQueries brings the namespace to its steady state and records its
// layout and, for pgvector, the plans of sample queries.
func prepareQueries(ctx context.Context, st *bench, ds *workload.Dataset, sets map[string]*querySet, cfg benchConfig) {
	logf("[%s D=%d] bringing the namespace to a steady state", st.res.Backend, ds.Dim)
	note, err := st.b.Ready(ctx, st.ns)
	if err != nil {
		st.fail("ready", err)
		return
	}
	st.res.Ready = note
	st.res.StoreInfo = st.b.Describe(ctx, st.ns)
	if st.b.Explain != nil {
		samples := map[string]backend.SampleQuery{}
		for op, s := range sets {
			if len(s.nearest) > 0 {
				q := s.nearest[0]
				samples[op] = backend.SampleQuery{Vector: q.Vector, Filters: ds.Partitions[q.Partition].Filters(), Threshold: cfg.Threshold, Limit: 1}
			}
		}
		st.res.Plans = st.b.Explain(ctx, st.ns, samples)
	}
}

// measureQueries runs the warm-up and then the measured phase of one query op.
func measureQueries(ctx context.Context, st *bench, ds *workload.Dataset, s *querySet, cfg benchConfig, op string, run, c int) {
	warm := &Tally{}
	stats.RunPhase(warmLen(s), c, func(i int) error { return execOp(ctx, st.store, st.ns, ds, cfg, op, s, true, i, warm) })
	tally := &Tally{}
	s0 := cpuload.Read()
	pr := stats.RunPhase(measuredLen(s), c, func(i int) error { return execOp(ctx, st.store, st.ns, ds, cfg, op, s, false, i, tally) })
	busy := cpuload.BusyPercent(s0, cpuload.Read())
	tally.finish()
	rec := PhaseRecord{Op: op, Concurrency: c, Run: run, Summary: pr.Summary(), FirstError: errText(pr.FirstErr), Correctness: tally, CPUBusyPct: busy}
	st.res.Phases = append(st.res.Phases, rec)
	logf("[%s D=%d] run %d %s c=%d: p50 %.3f p95 %.3f p99 %.3f ms, %.0f ops/s, errors %d%s%s", st.res.Backend, ds.Dim, run, op, c,
		rec.Summary.P50Ms, rec.Summary.P95Ms, rec.Summary.P99Ms, rec.Summary.Throughput, rec.Summary.Errors, shortTally(op, tally), cpuText(busy))
}

// finishBackend computes medians and correctness totals, then drops the
// namespace (unless -keep) and closes the connections.
func finishBackend(st *bench, cfg benchConfig) {
	st.res.Medians = medians(st.res.Phases)
	for _, p := range st.res.Phases {
		if p.Correctness == nil {
			continue
		}
		t, ok := st.res.Correctness[p.Op]
		if !ok {
			t = &Tally{}
			st.res.Correctness[p.Op] = t
		}
		t.merge(p.Correctness)
	}
	for _, t := range st.res.Correctness {
		t.finish()
	}
	if st.b == nil {
		return
	}
	if !cfg.Keep && st.res.CreateNamespaceMs > 0 {
		if err := st.store.DeleteNamespace(context.Background(), st.ns); err != nil {
			logf("[%s D=%d] drop namespace: %v", st.res.Backend, st.res.Dim, err)
		} else {
			st.res.Dropped = true
		}
	}
	st.b.Close()
	logf("[%s D=%d] done", st.res.Backend, st.res.Dim)
}

func cpuText(busy *float64) string {
	if busy == nil {
		return ""
	}
	return fmt.Sprintf(", VM CPU busy %.0f%%", *busy)
}
