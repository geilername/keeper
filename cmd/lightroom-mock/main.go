// Command lightroom-mock is an in-memory, Lightroom-shaped catalogue for the
// publisher to write to, with admin endpoints that inject 5xx errors and
// latency for the fault demo (HACKATHON-PLAN.md §4).
//
// API (paths as in Adobe's Lightroom Partner API, lr.adobe.io/v2):
//
//	GET   /v2/catalog                                       catalogue ID
//	GET   /v2/catalogs/{catalog_id}/assets                  list (+count)
//	PUT   /v2/catalogs/{catalog_id}/assets/{asset_id}        create, idempotent on asset_id
//	GET   /v2/catalogs/{catalog_id}/assets/{asset_id}        get
//	PATCH /v2/catalogs/{catalog_id}/assets/{asset_id}        {"rating":1..5} (mock-only shape)
//	PUT   /v2/catalogs/{catalog_id}/assets/{asset_id}/master original bytes
//
// Admin (never faulted): GET/POST /admin/faults, POST /admin/reset,
// GET /admin/stats, GET /healthz.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"github.com/geilername/keeper/internal/telemetry"
)

const (
	service = "lightroom-mock"
	// Fixed so the publisher and curl can hard-code it; override with
	// LR_CATALOG_ID. A real adapter reads it from GET /v2/catalog.
	defaultCatalogID = "00000000000000000000000000000001"
)

func main() {
	// Distroless has no curl, so the compose healthcheck runs the binary.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, service)
	if err != nil {
		slog.Error("telemetry setup failed", "err", err)
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

	s := newServer(envOr("LR_CATALOG_ID", defaultCatalogID), otel.Meter(service))
	srv := &http.Server{
		Addr: addr(),
		Handler: otelhttp.NewHandler(s.handler(), service,
			otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }),
			// Runs again after routing, so spans get "PUT /v2/catalogs/{catalog_id}/…".
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if r.Pattern != "" {
					return r.Pattern
				}
				return r.Method
			})),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	slog.InfoContext(ctx, "lightroom-mock listening", "addr", srv.Addr, "catalog_id", s.catalogID)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server failed", "err", err)
	}
}

func healthcheck() int {
	_, port, err := net.SplitHostPort(addr())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		fmt.Fprintln(os.Stderr, "healthz:", resp.Status)
		return 1
	}
	return 0
}

func addr() string { return envOr("HTTP_ADDR", ":8080") }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
