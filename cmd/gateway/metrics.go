package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/geilername/keeper/internal/events"
)

const meterName = "github.com/geilername/keeper/cmd/gateway"

// consumers whose backlog keeper.consumer.pending reports, one series each.
var consumers = []string{"scorer", "culler", "publisher", "gateway"}

type metrics struct {
	e2e metric.Float64Histogram
	reg metric.Registration
}

// newMetrics registers the gateway's metrics (§5.2). Labels are low
// cardinality on purpose: no sha, path or file name.
func newMetrics(js jetstream.JetStream) (*metrics, error) {
	meter := otel.Meter(meterName)
	m := &metrics{}
	var err error

	// The gateway is the one service that sees both ends of a photo's life,
	// so it records end-to-end time. Dash0 shows a trace's duration as its
	// root span's, which ends long before the photo is published.
	m.e2e, err = meter.Float64Histogram("keeper.photo.e2e.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time from capture to the photo.published event"),
		metric.WithExplicitBucketBoundaries(1, 2, 5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 300, 600),
	)
	if err != nil {
		return nil, err
	}

	pending, err := meter.Int64ObservableGauge("keeper.consumer.pending",
		metric.WithUnit("{message}"),
		metric.WithDescription("Messages in the stream not yet delivered to the consumer"),
	)
	if err != nil {
		return nil, err
	}
	m.reg, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		observePending(ctx, js, func(name string, n int64) {
			o.ObserveInt64(pending, n, metric.WithAttributes(attribute.String("consumer", name)))
		})
		return nil
	}, pending)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// observePending reads JetStream consumer info for each durable. A consumer
// that does not exist yet (its service never started) is skipped rather than
// reported as 0: "no backlog" and "nobody consuming" are different things.
func observePending(ctx context.Context, js jetstream.JetStream, observe func(string, int64)) {
	for _, name := range consumers {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		c, err := js.Consumer(cctx, events.Stream, name)
		cancel()
		switch {
		case errors.Is(err, jetstream.ErrConsumerNotFound):
			continue
		case err != nil:
			slog.WarnContext(ctx, "consumer info failed", "consumer", name, "err", err)
			continue
		}
		observe(name, int64(c.CachedInfo().NumPending))
	}
}

func (m *metrics) recordE2E(ctx context.Context, d time.Duration) {
	if d < 0 {
		return // clock skew between camera and stream; not a real duration
	}
	m.e2e.Record(ctx, d.Seconds())
}

func (m *metrics) Close() error { return m.reg.Unregister() }
