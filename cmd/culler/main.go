// Command culler groups scored photos into bursts and decides, per burst,
// which frame is best (5★), which are keepers (3★) and which are rejects
// (1★). Open burst windows live only in memory; their photo.scored messages
// stay unacked until the window is decided, so a restart rebuilds them from
// JetStream redelivery (ADR-4). The rules themselves are in internal/policy.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/telemetry"
)

const service = "culler"

// ackWait is how long JetStream waits before redelivering a held message.
// Held messages get InProgress every ackWait/3, so it can stay short: after
// a crash, the open windows come back within ackWait.
const ackWait = 30 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, service)
	if err != nil {
		slog.Error("telemetry setup failed", "err", err)
		os.Exit(1)
	}

	err = run(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "culler failed", "err", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = shutdown(sctx) // flush telemetry
	cancel()
	if err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	natsURL := env("NATS_URL", "nats://127.0.0.1:4222")

	nc, js, err := natsx.Connect(ctx, natsURL, service)
	if err != nil {
		return err
	}
	defer nc.Close()

	slog.InfoContext(ctx, "culler starting", "nats", natsURL, "burst_gap", cfg.Gap,
		"dhash_max", cfg.MaxHamming, "burst_idle", cfg.Idle, "burst_max_wait", cfg.MaxWait)
	return serve(ctx, js, cfg, ackWait)
}

// serve consumes photo.scored and closes windows until ctx ends. Open
// windows are left unacked on the way out.
func serve(ctx context.Context, js jetstream.JetStream, cfg windowConfig, ackWait time.Duration) error {
	c, err := newCuller(cfg, ackWait, func(ctx context.Context, subject, msgID string, v any) error {
		return natsx.Publish(ctx, js, subject, msgID, v)
	})
	if err != nil {
		return err
	}

	done := make(chan struct{})
	go func() { defer close(done); c.run(ctx) }()

	err = natsx.Consume(ctx, js, natsx.ConsumerConfig{
		Durable:       service,
		FilterSubject: events.SubjectScored,
		AckWait:       ackWait,
	}, func(p events.PhotoScored) string { return p.SHA256 }, c.handle)

	<-done
	c.release(context.WithoutCancel(ctx))
	return err
}

func configFromEnv() (windowConfig, error) {
	var cfg windowConfig
	var err error
	if cfg.Gap, err = durationEnv("BURST_GAP", 2*time.Second); err != nil {
		return cfg, err
	}
	if cfg.TightGap, err = durationEnv("BURST_TIGHT_GAP", time.Second); err != nil {
		return cfg, err
	}
	if cfg.Idle, err = durationEnv("BURST_IDLE", 20*time.Second); err != nil {
		return cfg, err
	}
	if cfg.MaxWait, err = durationEnv("BURST_MAX_WAIT", 90*time.Second); err != nil {
		return cfg, err
	}
	if cfg.MaxHamming, err = strconv.Atoi(env("DHASH_MAX", "12")); err != nil {
		return cfg, fmt.Errorf("DHASH_MAX: %w", err)
	}
	return cfg, nil
}

func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
