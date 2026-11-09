// Command indexer keeps an Elasticsearch index of every photo, for search.
// It consumes the photo.* events of the PHOTOS stream with its own durable
// and folds them with the same code as the gateway's Postgres read model
// (internal/photostate), one document per photo. The index is derived data:
// delete it and restart the indexer, and the stream refills it.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/search"
	"github.com/geilername/keeper/internal/telemetry"
)

const service = "indexer"

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
	err = run(ctx, env("NATS_URL", "nats://nats:4222"), env("ELASTICSEARCH_URL", "http://elasticsearch:9200"))
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = shutdown(sctx)
	if err != nil {
		slog.Error("indexer failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, natsURL, esURL string) error {
	nc, js, err := natsx.Connect(ctx, natsURL, service)
	if err != nil {
		return err
	}
	defer nc.Close()

	es := search.New(esURL, 10*time.Second)
	if err := ensureIndex(ctx, es, js); err != nil {
		return err
	}
	slog.InfoContext(ctx, "indexer consuming", "index", search.PhotosIndex)
	ix := &indexer{es: es, now: time.Now}
	if err := ix.run(ctx, js); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
