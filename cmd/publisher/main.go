// Command publisher consumes photo.decided and puts each photo into
// Lightroom: create the asset, upload the original from /spool, set the
// rating and, with Adobe, add it to the collection picked in the console.
// It then publishes photo.published, or photo.failed per failed attempt.
//
// LIGHTROOM_MODE picks the backend: "mock" (default) talks to
// cmd/lightroom-mock at LIGHTROOM_URL; "adobe" talks to lr.adobe.io with
// ADOBE_CLIENT_ID as API key and takes the access token and the target
// collection from the keeper-lightroom bucket (internal/lrlink), sealed with
// KEEPER_TOKEN_KEY. Retries are JetStream redeliveries with backoff (ADR-1); every
// step is idempotent, so a replay after a crash ends in the same state
// (ADR-2).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/lrlink"
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
	// AckWait stays at natsx's 30s; handle tells JetStream the message is
	// in progress every 10s, so a long Adobe upload is not redelivered
	// while it runs.
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
	mode := envOr("LIGHTROOM_MODE", "mock")
	if mode != "mock" && mode != "adobe" {
		return fmt.Errorf("LIGHTROOM_MODE=%q: want mock or adobe", mode)
	}
	// Adobe is across the internet: longer calls, and uploads of raws in
	// minutes (the handler keeps the message alive meanwhile).
	defTimeout, defUpload := "5s", "5s"
	if mode == "adobe" {
		defTimeout, defUpload = "15s", "5m"
	}
	timeout, err := time.ParseDuration(envOr("LIGHTROOM_TIMEOUT", defTimeout))
	if err != nil {
		return err
	}
	uploadTimeout, err := time.ParseDuration(envOr("LIGHTROOM_UPLOAD_TIMEOUT", defUpload))
	if err != nil {
		return err
	}

	nc, js, err := natsx.Connect(ctx, envOr("NATS_URL", "nats://nats:4222"), service)
	if err != nil {
		return err
	}
	defer nc.Close()

	emit := func(ctx context.Context, subject, msgID string, v any) error {
		return natsx.Publish(ctx, js, subject, msgID, v)
	}

	var p *publisher
	switch mode {
	case "mock":
		lr := lightroom.NewHTTP(
			envOr("LIGHTROOM_URL", "http://lightroom-mock:8080"),
			envOr("LR_CATALOG_ID", lightroom.DefaultCatalogID),
			timeout)
		if p, err = newPublisher(lr, emit, consumerConfig, otel.Tracer(service), otel.Meter(service)); err != nil {
			return err
		}
	case "adobe":
		apiKey := os.Getenv("ADOBE_CLIENT_ID")
		if apiKey == "" {
			return errors.New("LIGHTROOM_MODE=adobe needs ADOBE_CLIENT_ID")
		}
		sealer, err := lrlink.NewSealer(os.Getenv("KEEPER_TOKEN_KEY"))
		if err != nil {
			return err
		}
		kv, err := lrlink.OpenBucket(ctx, js)
		if err != nil {
			return fmt.Errorf("open %s bucket: %w", lrlink.Bucket, err)
		}
		link := lrlink.NewLink(sealer)
		lr := lightroom.NewAdobe(envOr("LIGHTROOM_URL", lightroom.DefaultAdobeURL), apiKey, link, timeout)
		lr.SetUploadTimeout(uploadTimeout)
		link.OnAccountChange(lr.Reset)
		go func() {
			// The watcher reconnects with NATS; if it ever stops, restart it.
			for ctx.Err() == nil {
				if err := link.Watch(ctx, kv); err != nil && ctx.Err() == nil {
					slog.WarnContext(ctx, "lightroom link watch stopped, restarting", "err", err)
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
					}
				}
			}
		}()
		if p, err = newPublisher(lr, emit, consumerConfig, otel.Tracer(service), otel.Meter(service)); err != nil {
			return err
		}
		p.withAlbums(lr, link)
	}

	slog.InfoContext(ctx, "publisher consuming", "subject", events.SubjectDecided,
		"lightroom_mode", mode, "lightroom_timeout", timeout)
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
