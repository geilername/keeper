// Command publisher consumes photo.decided and puts each photo into
// Lightroom: create the asset, upload the original from /spool, set the
// rating. It then publishes photo.published, or photo.failed per failed
// attempt. Retries are JetStream redeliveries with backoff (ADR-1); every
// step is idempotent, so a replay after a crash ends in the same state
// (ADR-2).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/telemetry"
)

const service = "publisher"

// consumerConfig: 2s, 5s, 15s, then every 30s for as long as Lightroom is
// down. Retryable errors never exhaust the message (MaxDeliver -1): an outage
// delays photos, it never drops them. Only permanent errors (a 4xx conflict)
// end in "not published".
var consumerConfig = natsx.ConsumerConfig{
	Durable:       service,
	FilterSubject: events.SubjectDecided,
	// AckWait stays at natsx's 30s: one attempt is at most three Lightroom
	// calls of LIGHTROOM_TIMEOUT (5s) each.
	MaxDeliver: -1,
	BackOff:    []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second},
}

func main() { os.Exit(mainCode()) }

// mainCode exists so the deferred telemetry flush runs before os.Exit.
func mainCode() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, service)
	if err != nil {
		slog.Error("telemetry setup failed", "err", err)
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

	if err := run(ctx); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "publisher failed", "err", err)
		return 1
	}
	slog.InfoContext(ctx, "publisher stopped")
	return 0
}

func run(ctx context.Context) error {
	timeout, err := time.ParseDuration(envOr("LIGHTROOM_TIMEOUT", "5s"))
	if err != nil {
		return err
	}
	lr := lightroom.NewHTTP(
		envOr("LIGHTROOM_URL", "http://lightroom-mock:8080"),
		envOr("LR_CATALOG_ID", lightroom.DefaultCatalogID),
		timeout)

	nc, js, err := natsx.Connect(ctx, envOr("NATS_URL", "nats://nats:4222"), service)
	if err != nil {
		return err
	}
	defer nc.Close()

	emit := func(ctx context.Context, subject, msgID string, v any) error {
		return natsx.Publish(ctx, js, subject, msgID, v)
	}
	p, err := newPublisher(lr, emit, consumerConfig, otel.Tracer(service), otel.Meter(service))
	if err != nil {
		return err
	}

	slog.InfoContext(ctx, "publisher consuming", "subject", events.SubjectDecided, "lightroom_timeout", timeout)
	// Returns after ctx ends and the in-flight message is done; an attempt
	// cut off by shutdown is nak'd and replayed after restart.
	return natsx.Consume(ctx, js, consumerConfig,
		func(e events.PhotoDecided) string { return e.SHA256 }, p.handle)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
