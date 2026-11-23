// Telepathy — an OpenTelemetry-native observability platform in a single binary.
//
//	telepathy serve [--demo]   run the server (OTLP ingest + API + UI)
//	telepathy demo             send simulated telemetry to a running server
//	telepathy bench            measure ingest throughput and query latency
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gthimmes/telepathy/internal/alert"
	"github.com/gthimmes/telepathy/internal/api"
	"github.com/gthimmes/telepathy/internal/auth"
	"github.com/gthimmes/telepathy/internal/demo"
	"github.com/gthimmes/telepathy/internal/ingest"
	"github.com/gthimmes/telepathy/internal/store"
	"github.com/gthimmes/telepathy/internal/ui"
)

var version = "0.2.0-dev"

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
	case "bench":
		err = runBench(ctx, os.Args[2:])
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
  telepathy serve [flags]   run the server (UI, API and OTLP/HTTP on :4318, OTLP/gRPC on :4317)
  telepathy demo  [flags]   send simulated telemetry to a running server
  telepathy bench [flags]   measure ingest throughput and query latency on this machine
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
	grpcAddr := fs.String("grpc-addr", env("TELEPATHY_GRPC_ADDR", ":4317"), `OTLP/gRPC listen address ("off" to disable)`)
	dbPath := fs.String("db", env("TELEPATHY_DB", "telepathy.db"), "database file")
	retention := fs.Duration("retention", 7*24*time.Hour, "how long to keep telemetry")
	dashDir := fs.String("dashboards", env("TELEPATHY_DASHBOARDS", "dashboards"), "directory of dashboards-as-code (*.json)")
	locKeys := fs.String("location-keys", env("TELEPATHY_LOCATION_KEYS", ""), "comma-separated resource attributes that define a location, in priority order")
	adminPw := fs.String("admin-password", env("TELEPATHY_ADMIN_PASSWORD", ""), "turn on authentication with this admin password (prefer the TELEPATHY_ADMIN_PASSWORD environment variable)")
	ingestKey := fs.String("ingest-key", env("TELEPATHY_INGEST_KEY", ""), "a static key accepted on OTLP ingest (Authorization: Bearer …)")
	publicURL := fs.String("public-url", env("TELEPATHY_PUBLIC_URL", ""), "URL people use to reach this server, for links in notifications")
	noAlerts := fs.Bool("no-alerts", false, "do not evaluate monitors or send notifications")
	withDemo := fs.Bool("demo", false, "generate simulated telemetry so there is something to look at")
	fs.Parse(args)
	grpcExplicit := os.Getenv("TELEPATHY_GRPC_ADDR") != ""
	fs.Visit(func(f *flag.Flag) { grpcExplicit = grpcExplicit || f.Name == "grpc-addr" })

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

	host := *addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	if *publicURL == "" {
		*publicURL = "http://" + host
	}

	au := &auth.Auth{Store: st, AdminPassword: *adminPw, IngestKey: *ingestKey}
	if *withDemo && au.Enabled() && au.IngestKey == "" {
		// The built-in demo sends real OTLP to this server; give it a private credential.
		b := make([]byte, 24)
		rand.Read(b)
		au.IngestKey = hex.EncodeToString(b)
	}

	var engine *alert.Engine
	if !*noAlerts {
		if err := alert.EnsureDefaults(st); err != nil {
			return err
		}
		engine = &alert.Engine{Store: st, PublicURL: *publicURL, Logf: log.Printf}
		go engine.Run(ctx.Done())
	}

	mux := http.NewServeMux()
	ing := &ingest.Handler{Store: st}
	if *locKeys != "" {
		ing.LocationKeys = strings.Split(*locKeys, ",")
	}
	ing.Register(mux)
	au.Register(mux)
	(&api.API{Store: st, Version: version, Alerts: engine}).Register(mux)
	mux.Handle("/", ui.Handler())

	srv := &http.Server{Addr: *addr, Handler: au.Middleware(mux), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	if *grpcAddr != "" && *grpcAddr != "off" {
		lis, err := net.Listen("tcp", *grpcAddr)
		switch {
		case err == nil:
			gs := ing.GRPCServer(au.AllowIngest)
			go gs.Serve(lis)
			go func() { <-ctx.Done(); gs.Stop() }()
			log.Printf("OTLP/gRPC listening on %s", *grpcAddr)
		case grpcExplicit:
			return fmt.Errorf("OTLP/gRPC listener: %w", err)
		default:
			log.Printf("OTLP/gRPC disabled: %v (use --grpc-addr to choose another port)", err)
		}
	}

	go func() { // retention
		for {
			if err := st.Prune(time.Now().Add(-*retention).UnixNano()); err != nil {
				log.Printf("retention: %v", err)
			}
			st.PruneAlerts(time.Now().Add(-30 * 24 * time.Hour).UnixMilli())
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
			}
		}
	}()

	log.Printf("telepathy %s listening on http://%s  (UI, API, and OTLP at /v1/{traces,logs,metrics})", version, host)
	if !au.Enabled() {
		log.Print("authentication is OFF: anyone who can reach this port can read and change everything. Set TELEPATHY_ADMIN_PASSWORD to turn it on.")
	}

	if *withDemo {
		backfill := time.Hour
		if latest := st.LatestSpanNs(); latest > 0 {
			if gap := time.Since(time.Unix(0, latest)); gap < backfill {
				backfill = gap.Truncate(time.Second)
			}
		}
		go func() {
			time.Sleep(300 * time.Millisecond)
			if err := demo.Run(ctx, demo.Options{Endpoint: "http://" + host, Backfill: backfill, APIKey: au.IngestKey, Logf: log.Printf}); err != nil {
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
	key := fs.String("ingest-key", env("TELEPATHY_INGEST_KEY", ""), "ingest key or API token, if the server requires one")
	fs.Parse(args)
	return demo.Run(ctx, demo.Options{Endpoint: *endpoint, Backfill: *backfill, Rate: *rate, APIKey: *key, Logf: log.Printf})
}
