package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/policy"
	"github.com/geilername/keeper/internal/telemetry"
)

const instrumentation = "github.com/geilername/keeper/cmd/culler"

// forgetAfter is how long decided photos are remembered, so that a
// redelivery after a lost ack is recognised. Well above AckWait.
const forgetAfter = 10 * time.Minute

// publishFunc is natsx.Publish bound to a JetStream handle (a fake in tests).
type publishFunc func(ctx context.Context, subject, msgID string, v any) error

// culler holds the burst windows. handle runs on the consumer goroutine,
// tick on the ticker goroutine; mu serialises them. A window is decided and
// acked while mu is held, so no photo can join a window that is closing.
type culler struct {
	publish publishFunc
	ackWait time.Duration

	mu           sync.Mutex
	w            *windows
	lastProgress time.Time

	tracer    trace.Tracer
	decisions metric.Int64Counter
	burstOpen metric.Int64UpDownCounter
	stageDur  metric.Float64Histogram
}

func newCuller(cfg windowConfig, ackWait time.Duration, publish publishFunc) (*culler, error) {
	meter := otel.Meter(instrumentation)
	decisions, err := meter.Int64Counter("keeper.decisions",
		metric.WithDescription("Culling decisions per bucket"), metric.WithUnit("{photo}"))
	if err != nil {
		return nil, err
	}
	burstOpen, err := meter.Int64UpDownCounter("keeper.burst.open",
		metric.WithDescription("Burst windows open right now"), metric.WithUnit("{burst}"))
	if err != nil {
		return nil, err
	}
	stageDur, err := meter.Float64Histogram("keeper.stage.duration",
		metric.WithDescription("Time spent per pipeline stage"), metric.WithUnit("s"),
		// cull = time a photo waits in its burst window: up to BURST_MAX_WAIT.
		metric.WithExplicitBucketBoundaries(0.5, 1, 2.5, 5, 10, 20, 30, 60, 90, 120))
	if err != nil {
		return nil, err
	}
	return &culler{
		publish: publish, ackWait: ackWait, w: newWindows(cfg), lastProgress: time.Now(),
		tracer: otel.Tracer(instrumentation), decisions: decisions, burstOpen: burstOpen, stageDur: stageDur,
	}, nil
}

// handle is the natsx.Consume handler for photo.scored. It never acks
// directly: the photo is held (ErrHold) until its window is decided.
func (c *culler) handle(ctx context.Context, m natsx.Msg, p events.PhotoScored) error {
	mem := &member{
		frame:  policy.Frame{SHA256: p.SHA256, CapturedAt: p.CapturedAt, Features: p.Features},
		msg:    m.Msg,
		parent: trace.SpanContextFromContext(ctx),
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.w.add(mem, time.Now()) {
	case alreadyDone:
		slog.InfoContext(ctx, "photo already decided, acking redelivery", "sha256", p.SHA256)
		return nil
	case opened:
		c.burstOpen.Add(ctx, 1)
		slog.InfoContext(ctx, "burst window opened", "sha256", p.SHA256, "open_windows", len(c.w.open))
	case joined:
		slog.InfoContext(ctx, "photo joined burst window", "sha256", p.SHA256)
	case redelivered:
		slog.InfoContext(ctx, "photo redelivered, already in a window", "sha256", p.SHA256, "delivery", m.Delivered)
	}
	return natsx.ErrHold
}

// run closes due windows every second until ctx ends.
func (c *culler) run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.tick(ctx, now)
		}
	}
}

func (c *culler) tick(ctx context.Context, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, d := range c.w.due(now) {
		if err := c.decide(ctx, d.win, d.reason, now); err != nil {
			// Nothing acked: the window stays open and the next tick
			// retries. Msg IDs are deterministic, so republishing is safe.
			slog.WarnContext(ctx, "deciding burst failed, will retry", "err", err)
			continue
		}
		c.w.remove(d.win, now)
		c.burstOpen.Add(ctx, -1)
	}
	c.w.forget(now, forgetAfter)

	// Keep held messages from being redelivered while their window is open.
	if now.Sub(c.lastProgress) >= c.ackWait/3 {
		for _, m := range c.w.bySHA {
			if err := m.msg.InProgress(); err != nil {
				slog.WarnContext(ctx, "in-progress failed", "sha256", m.frame.SHA256, "err", err)
			}
		}
		c.lastProgress = now
	}
}

// decide applies the policy to one window, publishes burst.decided and one
// photo.decided per member, then acks the held messages.
func (c *culler) decide(ctx context.Context, win *window, closeReason string, now time.Time) error {
	members := slices.Clone(win.members)
	slices.SortFunc(members, func(a, b *member) int {
		if r := a.frame.CapturedAt.Compare(b.frame.CapturedAt); r != 0 {
			return r
		}
		return cmp.Compare(a.frame.SHA256, b.frame.SHA256)
	})
	frames := make([]policy.Frame, len(members))
	shas := make([]string, len(members))
	links := make([]trace.Link, len(members))
	for i, m := range members {
		frames[i], shas[i] = m.frame, m.frame.SHA256
		links[i] = trace.Link{SpanContext: trace.SpanContextFromContext(telemetry.PhotoRootContext(ctx, shas[i]))}
	}
	res := policy.Decide(frames)
	burstID := shas[0] // the first frame in capture order names the burst

	// The burst gets a trace of its own, linked to every member photo.
	bctx, bspan := c.tracer.Start(ctx, "burst.decide", trace.WithNewRoot(), trace.WithLinks(links...),
		trace.WithAttributes(
			attribute.String("keeper.burst.id", burstID),
			attribute.Int("keeper.burst.size", len(members)),
			attribute.String("keeper.burst.best", res.Best),
			attribute.String("keeper.burst.close_reason", closeReason),
		))
	defer bspan.End()

	err := c.publish(bctx, events.SubjectBurst, events.MsgID(events.SubjectBurst, burstID), events.BurstDecided{
		V: events.Version, BurstID: burstID, Members: shas, Best: res.Best,
		Reason: res.Reason, CloseReason: closeReason,
	})
	if err != nil {
		return fail(bspan, fmt.Errorf("publish burst.decided: %w", err))
	}

	for i, m := range members {
		if err := c.publishDecision(ctx, m, res.Decisions[i], burstID, bspan.SpanContext()); err != nil {
			return fail(bspan, err)
		}
	}

	// Everything is published: release the held messages.
	for i, m := range members {
		if err := m.msg.Ack(); err != nil {
			// The redelivery will hit w.decided and be acked then.
			slog.WarnContext(bctx, "ack failed", "sha256", m.frame.SHA256, "err", err)
		}
		bucket := attribute.String("bucket", res.Decisions[i].Bucket)
		c.decisions.Add(bctx, 1, metric.WithAttributes(bucket))
		c.stageDur.Record(bctx, now.Sub(m.arrived).Seconds(), metric.WithAttributes(attribute.String("stage", "cull")))
	}
	slog.InfoContext(bctx, "burst decided", "burst_id", burstID, "size", len(members),
		"best", res.Best, "reason", res.Reason, "close_reason", closeReason)
	return nil
}

// publishDecision emits the photo's decision span inside the photo's own
// trace (under the held delivery), linked back to the burst span.
func (c *culler) publishDecision(ctx context.Context, m *member, d policy.Decision, burstID string, burst trace.SpanContext) error {
	parent := telemetry.PhotoRootContext(ctx, m.frame.SHA256)
	if m.parent.IsValid() {
		parent = trace.ContextWithSpanContext(ctx, m.parent)
	}
	ctx, span := c.tracer.Start(parent, "decision", trace.WithLinks(trace.Link{SpanContext: burst}),
		trace.WithAttributes(
			attribute.String("keeper.photo.sha256", m.frame.SHA256),
			attribute.String("keeper.burst.id", burstID),
			attribute.String("keeper.bucket", d.Bucket),
			attribute.Int("keeper.rating", d.Rating),
			attribute.StringSlice("keeper.reasons", d.Reasons),
		))
	defer span.End()

	err := c.publish(ctx, events.SubjectDecided, events.MsgID(events.SubjectDecided, m.frame.SHA256), events.PhotoDecided{
		V: events.Version, SHA256: m.frame.SHA256, BurstID: burstID,
		Bucket: d.Bucket, Rating: d.Rating, Reasons: d.Reasons,
	})
	if err != nil {
		return fail(span, fmt.Errorf("publish photo.decided %s: %w", m.frame.SHA256, err))
	}
	slog.InfoContext(ctx, "photo decided", "sha256", m.frame.SHA256, "bucket", d.Bucket, "rating", d.Rating, "reasons", d.Reasons)
	return nil
}

// release runs at shutdown. Open windows stay unacked on purpose: JetStream
// redelivers them to the next culler, which rebuilds the windows (ADR-4).
func (c *culler) release(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.w.open); n > 0 {
		c.burstOpen.Add(ctx, -int64(n))
		slog.InfoContext(ctx, "leaving open burst windows unacked for redelivery", "windows", n, "photos", len(c.w.bySHA))
	}
}

func fail(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return err
}
