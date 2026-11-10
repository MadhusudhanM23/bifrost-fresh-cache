// Times the GetNearest transaction pattern (BEGIN; SET LOCAL; SELECT; ROLLBACK) against a
// single query over the same pgx pool, to the pgvector test container on localhost:5433.
package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func report(name string, ds []time.Duration) {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	slow := 0
	for _, d := range ds {
		if d > 20*time.Millisecond {
			slow++
		}
	}
	q := func(p float64) time.Duration { return ds[int(p*float64(len(ds)-1))] }
	fmt.Printf("%-34s n=%d p50=%-9v p90=%-9v p99=%-9v max=%-9v >20ms=%d\n", name, len(ds), q(0.5).Round(10*time.Microsecond), q(0.9).Round(10*time.Microsecond), q(0.99).Round(10*time.Microsecond), ds[len(ds)-1].Round(10*time.Microsecond), slow)
}

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://bifrost:bifrost_password@127.0.0.1:5433/bifrost?sslmode=disable&pool_max_conns=5")
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	single := func() error { var x int; return pool.QueryRow(ctx, "SELECT 1").Scan(&x) }
	tx4 := func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL hnsw.iterative_scan = strict_order"); err != nil {
			return err
		}
		var x int
		if err := tx.QueryRow(ctx, "SELECT 1").Scan(&x); err != nil {
			return err
		}
		return tx.Rollback(ctx)
	}
	for _, gap := range []time.Duration{0, 30 * time.Millisecond} {
		for _, c := range []struct {
			name string
			f    func() error
		}{{"single SELECT", single}, {"BEGIN+SET LOCAL+SELECT+ROLLBACK", tx4}} {
			_ = c.f() // warm
			var ds []time.Duration
			for i := 0; i < 150; i++ {
				t := time.Now()
				if err := c.f(); err != nil {
					panic(err)
				}
				ds = append(ds, time.Since(t))
				time.Sleep(gap)
			}
			report(fmt.Sprintf("%s gap=%v", c.name, gap), ds)
		}
	}
}
