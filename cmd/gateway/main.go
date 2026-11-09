// Command gateway is Keeper's read side. It consumes every event on the
// PHOTOS stream into a Postgres read model (schema "gateway") and serves it
// to the browser: REST, a live SSE feed and the embedded console. Search
// (/api/search) queries the Elasticsearch index cmd/indexer maintains.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/geilername/keeper/internal/lrlink"
	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/search"
	"github.com/geilername/keeper/internal/telemetry"
	"github.com/geilername/keeper/web"
)

const service = "gateway"

type config struct {
	natsURL     string
	databaseURL string
	httpAddr    string
	httpsAddr   string // optional TLS listener (the Adobe OAuth callback must be https)
	tlsCert     string
	tlsKey      string
	traceURL    string // Dash0 trace deep link template, see tracelink.go
	dataset     string // DASH0_DATASET, for the Dash0 view state
	demoAdmin   string // lightroom-mock admin base URL; empty = demo controls off
	esURL       string // Elasticsearch for /api/search; empty disables it
	lightroom   lrConfig
}

func loadConfig() config {
	return config{
		natsURL:     env("NATS_URL", "nats://nats:4222"),
		databaseURL: env("DATABASE_URL", "postgres://keeper:keeper@postgres:5432/keeper"),
		httpAddr:    env("HTTP_ADDR", ":8080"),
		httpsAddr:   os.Getenv("HTTPS_ADDR"),
		tlsCert:     os.Getenv("TLS_CERT_FILE"),
		tlsKey:      os.Getenv("TLS_KEY_FILE"),
		traceURL:    os.Getenv("DASH0_TRACE_URL"),
		dataset:     env("DASH0_DATASET", "keeper-demo"),
		demoAdmin:   demoAdminURL(),
		esURL:       os.Getenv("ELASTICSEARCH_URL"),
		lightroom:   loadLRConfig(),
	}
}

// demoAdminURL enables the console's fault panel only for DEMO_CONTROLS=1.
func demoAdminURL() string {
	if os.Getenv("DEMO_CONTROLS") != "1" {
		return ""
	}
	return env("LIGHTROOM_ADMIN_URL", "http://lightroom-mock:8080")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, service)
	if err != nil {
		slog.Error("telemetry setup failed", "err", err)
		os.Exit(1)
	}
	err = run(ctx, loadConfig())
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = shutdown(sctx)
	if err != nil {
		slog.Error("gateway failed", "err", err)
		os.Exit(1)
	}
}

// run wires everything and blocks until ctx ends. Shutdown order: stop
// accepting HTTP and close SSE streams, drain the NATS consumer, then close
// the database pool the consumer was writing to.
func run(ctx context.Context, cfg config) error {
	store, err := OpenStore(ctx, cfg.databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	nc, js, err := natsx.Connect(ctx, cfg.natsURL, service)
	if err != nil {
		return err
	}
	defer nc.Close()

	m, err := newMetrics(js)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()

	links := traceLinker{tmpl: cfg.traceURL, dataset: cfg.dataset}
	bc := NewBroadcaster()
	pr := &projector{store: store, bc: bc, metrics: m, links: links}
	a := &api{store: store, bc: bc, links: links, web: web.FS()}
	if cfg.demoAdmin != "" {
		a.demo = newDemoProxy(cfg.demoAdmin)
		slog.InfoContext(ctx, "demo controls enabled", "lightroom_admin", cfg.demoAdmin)
	}
	if cfg.esURL != "" {
		a.search = search.New(cfg.esURL, 5*time.Second)
	}
	a.lrMode = cfg.lightroom.mode
	if cfg.lightroom.configured() {
		kv, err := lrlink.OpenBucket(ctx, js)
		if err != nil {
			return fmt.Errorf("open %s bucket: %w", lrlink.Bucket, err)
		}
		sealer, err := lrlink.NewSealer(cfg.lightroom.tokenKey)
		if err != nil {
			return err // client ID and secret without a key would store tokens in the clear
		}
		a.lr, err = newLightroomAuth(cfg.lightroom, store, lrlink.Writer{KV: kv, Sealer: sealer})
		if err != nil {
			return err
		}
		slog.InfoContext(ctx, "adobe lightroom configured", "redirect_uri", cfg.lightroom.redirectURI,
			"lightroom_mode", cfg.lightroom.mode)
	}

	srv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: SSE streams are long-lived; ServeSSE sets a
		// deadline per write instead.
	}
	srv.RegisterOnShutdown(bc.Close)

	// Optional second listener with TLS, same handler. Serving TLS needs
	// both files; a half-configured TLS listener is a startup error.
	var tlsSrv *http.Server
	if cfg.httpsAddr != "" {
		if cfg.tlsCert == "" || cfg.tlsKey == "" {
			return errors.New("HTTPS_ADDR needs TLS_CERT_FILE and TLS_KEY_FILE")
		}
		tlsSrv = &http.Server{Addr: cfg.httpsAddr, Handler: srv.Handler, ReadHeaderTimeout: 5 * time.Second}
		tlsSrv.RegisterOnShutdown(bc.Close)
	}

	// Either half failing stops the other one.
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	consumerDone := make(chan error, 1)
	go func() { consumerDone <- pr.run(ctx, js) }()
	if a.lr != nil {
		go a.lr.maintain(ctx, time.Minute)
	}

	httpDone := make(chan error, 2)
	go func() {
		slog.InfoContext(ctx, "gateway listening", "addr", cfg.httpAddr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		httpDone <- err
	}()
	if tlsSrv != nil {
		go func() {
			slog.InfoContext(ctx, "gateway listening with TLS", "addr", cfg.httpsAddr)
			err := tlsSrv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			httpDone <- err
		}()
	}

	var httpErr, consumerErr error
	consumerFinished := false
	select {
	case <-ctx.Done():
	case httpErr = <-httpDone:
	case consumerErr = <-consumerDone:
		consumerFinished = true
	}
	slog.InfoContext(ctx, "gateway shutting down")
	cancelRun()

	// Keep trace/log values, drop the cancellation, bound the wait.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx) // closes SSE streams via RegisterOnShutdown
	if tlsSrv != nil {
		_ = tlsSrv.Shutdown(sctx)
	}

	if !consumerFinished {
		consumerErr = <-consumerDone // drains in-flight messages, then returns
	}
	if errors.Is(consumerErr, context.Canceled) {
		consumerErr = nil
	}
	return errors.Join(httpErr, consumerErr)
}
