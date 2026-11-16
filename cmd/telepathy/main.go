// Telepathy — an OpenTelemetry-native observability platform in a single binary.
//
//	telepathy serve [--demo]   run the server (OTLP ingest + API + UI)
//	telepathy demo             send simulated telemetry to a running server
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gthimmes/telepathy/internal/api"
	"github.com/gthimmes/telepathy/internal/demo"
	"github.com/gthimmes/telepathy/internal/ingest"
	"github.com/gthimmes/telepathy/internal/store"
	"github.com/gthimmes/telepathy/internal/ui"
)

var version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "demo":
		err = runDemo(ctx, os.Args[2:])
	case "version", "--version":
		fmt.Println("telepathy", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `telepathy — OpenTelemetry-native observability in a single binary

Usage:
  telepathy serve [flags]   run the server (OTLP/HTTP ingest, API and UI on one port)
  telepathy demo  [flags]   send simulated telemetry to a running server
  telepathy version

Run "telepathy <command> -h" for flags.
`)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", env("TELEPATHY_ADDR", ":4318"), "listen address (4318 is the OTLP/HTTP default port)")
	dbPath := fs.String("db", env("TELEPATHY_DB", "telepathy.db"), "database file")
	retention := fs.Duration("retention", 7*24*time.Hour, "how long to keep telemetry")
	dashDir := fs.String("dashboards", env("TELEPATHY_DASHBOARDS", "dashboards"), "directory of dashboards-as-code (*.json)")
	locKeys := fs.String("location-keys", env("TELEPATHY_LOCATION_KEYS", ""), "comma-separated resource attributes that define a location, in priority order")
	ingestKey := fs.String("ingest-key", env("TELEPATHY_INGEST_KEY", ""), "require this key on OTLP ingest (Authorization: Bearer …)")
	withDemo := fs.Bool("demo", false, "generate simulated telemetry so there is something to look at")
	fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	if _, err := os.Stat(*dashDir); err == nil {
		n, err := api.ProvisionDashboards(st, *dashDir)
		if err != nil {
			return err
		}
		if n > 0 {
			log.Printf("provisioned %d dashboard(s) from %s", n, *dashDir)
		}
	}

	mux := http.NewServeMux()
	ing := &ingest.Handler{Store: st, APIKey: *ingestKey}
	if *locKeys != "" {
		ing.LocationKeys = strings.Split(*locKeys, ",")
	}
	ing.Register(mux)
	(&api.API{Store: st, Version: version}).Register(mux)
	mux.Handle("/", ui.Handler())

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	go func() { // retention
		for {
			if err := st.Prune(time.Now().Add(-*retention).UnixNano()); err != nil {
				log.Printf("retention: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
			}
		}
	}()

	host := *addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	log.Printf("telepathy %s listening on http://%s  (UI, API, and OTLP at /v1/{traces,logs,metrics})", version, host)

	if *withDemo {
		backfill := time.Hour
		if latest := st.LatestSpanNs(); latest > 0 {
			if gap := time.Since(time.Unix(0, latest)); gap < backfill {
				backfill = gap.Truncate(time.Second)
			}
		}
		go func() {
			time.Sleep(300 * time.Millisecond)
			if err := demo.Run(ctx, demo.Options{Endpoint: "http://" + host, Backfill: backfill, APIKey: *ingestKey, Logf: log.Printf}); err != nil {
				log.Printf("demo: %v", err)
			}
		}()
	}

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Print("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

func runDemo(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	endpoint := fs.String("endpoint", env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318"), "Telepathy (or any OTLP/HTTP) endpoint")
	backfill := fs.Duration("backfill", time.Hour, "history to generate before going live")
	rate := fs.Int("rate", 6, "traces per second")
	key := fs.String("ingest-key", env("TELEPATHY_INGEST_KEY", ""), "ingest key, if the server requires one")
	fs.Parse(args)
	return demo.Run(ctx, demo.Options{Endpoint: *endpoint, Backfill: *backfill, Rate: *rate, APIKey: *key, Logf: log.Printf})
}
