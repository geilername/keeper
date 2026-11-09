// Command ingest receives SFTPGo upload hooks, content-addresses each photo
// and publishes photo.ingested (one trace per photo starts here).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/spool"
	"github.com/geilername/keeper/internal/telemetry"
)

const service = "ingest"

func main() {
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

	st, err := openStore(ctx, env("DATABASE_URL", "postgres://keeper:keeper@postgres:5432/keeper"))
	if err != nil {
		slog.Error("postgres unavailable", "err", err)
		os.Exit(1)
	}
	defer st.db.Close()
	nc, js, err := natsx.Connect(ctx, env("NATS_URL", "nats://nats:4222"), service)
	if err != nil {
		slog.Error("nats unavailable", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	in := newIngester(st, js)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/sftpgo", in.hook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	slog.InfoContext(ctx, "ingest listening", "addr", env("HTTP_ADDR", ":8080"), "incoming", spool.Incoming())

	srv := &http.Server{
		Addr: env("HTTP_ADDR", ":8080"),
		Handler: otelhttp.NewHandler(mux, service,
			otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" })),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server failed", "err", err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
