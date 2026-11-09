package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/natsx"
)

// projector turns stream events into read-model rows and SSE updates.
type projector struct {
	store   *Store
	bc      *Broadcaster
	metrics *metrics
	links   traceLinker
}

// run consumes every subject of the PHOTOS stream with the durable "gateway"
// until ctx ends. A new durable starts at the beginning of the stream, so an
// empty read model fills itself from history.
func (pr *projector) run(ctx context.Context, js jetstream.JetStream) error {
	return natsx.Consume(ctx, js, natsx.ConsumerConfig{
		Durable: "gateway", // no filter: photo.> and burst.>
		// The read model must not lose events: while Postgres is down, keep
		// retrying (with the BackOff delays) instead of giving up after 8.
		MaxDeliver: -1,
	}, rawSHA, pr.handle)
}

// rawSHA lets natsx put a message without traceparent into its photo trace.
func rawSHA(raw json.RawMessage) string {
	var v struct {
		SHA256 string `json:"sha256"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.SHA256
}

func (pr *projector) handle(ctx context.Context, m natsx.Msg, raw json.RawMessage) error {
	// The stream timestamp is the event time: stable across redeliveries.
	at := time.Now()
	if md, err := m.Metadata(); err == nil {
		at = md.Timestamp
	}

	subject := m.Subject()
	if subject == events.SubjectBurst {
		var b events.BurstDecided
		if err := json.Unmarshal(raw, &b); err != nil || b.BurstID == "" {
			return natsx.Permanent(fmt.Errorf("bad %s: %v", subject, err))
		}
		return pr.store.UpsertBurst(ctx, &b, at)
	}

	sha, ev, err := decodePhotoEvent(subject, raw)
	if err != nil {
		return natsx.Permanent(err)
	}
	if ev == nil {
		slog.InfoContext(ctx, "ignoring unknown subject", "subject", subject)
		return nil
	}

	p, changed, firstPublished, err := pr.store.ApplyPhotoEvent(ctx, sha, ev, at)
	if err != nil {
		return err // nak: redelivered with backoff
	}
	if firstPublished && p.CapturedAt != nil && p.PublishedAt != nil {
		pr.metrics.recordE2E(ctx, p.PublishedAt.Sub(*p.CapturedAt))
	}
	if changed {
		pr.bc.Publish(pr.links.snapshot(p))
	}
	return nil
}

// decodePhotoEvent decodes a photo.* payload into its contract type. It
// returns a nil event for subjects the gateway does not know.
func decodePhotoEvent(subject string, raw []byte) (string, any, error) {
	var (
		ev  any
		sha *string
	)
	switch subject {
	case events.SubjectIngested:
		e := &events.PhotoIngested{}
		ev, sha = e, &e.SHA256
	case events.SubjectScored:
		e := &events.PhotoScored{}
		ev, sha = e, &e.SHA256
	case events.SubjectDecided:
		e := &events.PhotoDecided{}
		ev, sha = e, &e.SHA256
	case events.SubjectPublished:
		e := &events.PhotoPublished{}
		ev, sha = e, &e.SHA256
	case events.SubjectFailed:
		e := &events.PhotoFailed{}
		ev, sha = e, &e.SHA256
	default:
		return "", nil, nil
	}
	if err := json.Unmarshal(raw, ev); err != nil {
		return "", nil, fmt.Errorf("decode %s: %w", subject, err)
	}
	if !validSHA(*sha) {
		return "", nil, fmt.Errorf("%s: invalid sha256 %q", subject, *sha)
	}
	return *sha, ev, nil
}
