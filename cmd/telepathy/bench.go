package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gthimmes/telepathy/internal/demo"
	"github.com/gthimmes/telepathy/internal/ingest"
	"github.com/gthimmes/telepathy/internal/query"
	"github.com/gthimmes/telepathy/internal/store"
)

// runBench is the load test: it pushes simulated telemetry through the real
// OTLP/HTTP ingest path into a fresh database as fast as it will go, then
// times the queries the UI depends on against that data.
func runBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	history := fs.Duration("history", time.Hour, "how much simulated history to ingest")
	rate := fs.Int("rate", 20, "simulated traces per second (about 6 spans each)")
	dbPath := fs.String("db", "", "database file to create (default: a temporary one, removed afterwards)")
	runs := fs.Int("runs", 5, "times to run each query (the median is reported)")
	fs.Parse(args)

	if *dbPath == "" {
		dir, err := os.MkdirTemp("", "telepathy-bench")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		*dbPath = filepath.Join(dir, "bench.db")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	mux := http.NewServeMux()
	(&ingest.Handler{Store: st}).Register(mux)
	mux.HandleFunc("POST /api/v1/events", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) })
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(lis)
	defer srv.Close()

	fmt.Printf("ingesting %s of history at %d traces/s over OTLP/HTTP...\n", *history, *rate)
	t0 := time.Now()
	if err := demo.Run(ctx, demo.Options{Endpoint: "http://" + lis.Addr().String(), Backfill: *history, Rate: *rate, Once: true}); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	took := time.Since(t0)
	count := func(table string) (n int64) {
		st.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
		return
	}
	spans, logs, metrics := count("spans"), count("logs"), count("metrics")
	size := int64(0)
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(*dbPath + suffix); err == nil {
			size += fi.Size()
		}
	}
	total := spans + logs + metrics
	fmt.Printf("\nIngest\n  %d spans, %d logs, %d metric points in %s\n  %.0f records/s (%.0f spans/s), %.0f MB on disk (%.0f bytes/record)\n",
		spans, logs, metrics, took.Round(time.Millisecond), float64(total)/took.Seconds(), float64(spans)/took.Seconds(),
		float64(size)/1e6, float64(size)/float64(max(total, 1)))

	rng := history.String()
	tr := query.TimeRange{FromNs: time.Now().Add(-*history).UnixNano(), ToNs: time.Now().UnixNano()}
	db := st.DB()
	queries := []struct {
		name string
		fn   func() error
	}{
		{"health matrix (5m windows)", func() error { _, err := query.Health(db, 5*time.Minute); return err }},
		{"request rate by service", func() error {
			_, err := query.Run(db, query.Request{Signal: query.Spans, Agg: "rate", Filter: "kind:server", GroupBy: []string{"service"}, Range: rng})
			return err
		}},
		{"p95 latency by location", func() error {
			_, err := query.Run(db, query.Request{Signal: query.Spans, Agg: "p95", Filter: "service:checkout kind:server", GroupBy: []string{"location"}, Range: rng})
			return err
		}},
		{"error rate by attribute", func() error {
			_, err := query.Run(db, query.Request{Signal: query.Spans, Agg: "error_rate", Filter: "service:payments", GroupBy: []string{"payment.provider"}, Range: rng})
			return err
		}},
		{"metric avg by location", func() error {
			_, err := query.Run(db, query.Request{Signal: query.Metrics, Metric: "system.cpu.utilization", Agg: "avg", GroupBy: []string{"location"}, Range: rng})
			return err
		}},
		{"explain (errors, payments)", func() error {
			_, err := query.Explain(db, st, query.ExplainRequest{Scope: "service:payments", Range: rng})
			return err
		}},
		{"service map", func() error { _, err := query.ServiceMap(db, tr, ""); return err }},
		{"span search by attribute", func() error {
			_, err := query.SearchSpans(db, "status:error payment.provider:adyen", tr, "recent", 100)
			return err
		}},
		{"log search", func() error { _, err := query.SearchLogs(db, "level:error", tr, 200); return err }},
	}
	fmt.Printf("\nQueries over %s (median of %d)\n", rng, *runs)
	for _, q := range queries {
		var ds []time.Duration
		for i := 0; i < *runs; i++ {
			s := time.Now()
			if err := q.fn(); err != nil {
				return fmt.Errorf("%s: %w", q.name, err)
			}
			ds = append(ds, time.Since(s))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		fmt.Printf("  %-30s %8s\n", q.name, ds[len(ds)/2].Round(time.Millisecond))
	}
	return nil
}
