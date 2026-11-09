package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/lrlink"
	"github.com/geilername/keeper/internal/media"
	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/spool"
)

// classSpool is the error_class when the original cannot be read from
// /spool. Ingest writes the file before it publishes photo.ingested, so this
// is a broken mount or permissions; it is retried so fixing the mount within
// the backoff budget still saves the photo.
const classSpool = "spool_unreadable"

// emitFunc publishes an event; in production it is natsx.Publish bound to
// the JetStream handle, in tests a recorder.
type emitFunc func(ctx context.Context, subject, msgID string, v any) error

// albumAdder and targetSource are the Adobe-only parts: where photos go
// (the collection picked in the console, from the keeper-lightroom bucket)
// and how they get there. Both are nil in mock mode, which has no albums.
type albumAdder interface {
	AddToAlbum(ctx context.Context, albumID string, assetIDs []string) error
}

type targetSource interface {
	Target() (lrlink.Target, bool)
}

type publisher struct {
	lr         lightroom.Client
	albums     albumAdder   // nil: no album step (mock)
	targets    targetSource // nil: no album step (mock)
	emit       emitFunc
	maxDeliver int
	backoff    []time.Duration // same slice natsx uses for nak delays
	now        func() time.Time

	tracer    trace.Tracer
	attempts  metric.Int64Counter
	photos    metric.Int64Counter
	duration  metric.Float64Histogram
	albumAdds metric.Int64Counter
}

func newPublisher(lr lightroom.Client, emit emitFunc, cfg natsx.ConsumerConfig,
	tracer trace.Tracer, meter metric.Meter,
) (*publisher, error) {
	attempts, err1 := meter.Int64Counter("keeper.publish.attempts",
		metric.WithDescription("Lightroom publish attempts by result (ok, retry, failed)"),
		metric.WithUnit("{attempt}"))
	photos, err2 := meter.Int64Counter("keeper.photos",
		metric.WithDescription("Photos by pipeline outcome"),
		metric.WithUnit("{photo}"))
	duration, err3 := meter.Float64Histogram("keeper.stage.duration",
		metric.WithDescription("Time per pipeline stage attempt"),
		metric.WithUnit("s"))
	albumAdds, err4 := meter.Int64Counter("keeper.lightroom.album.adds",
		metric.WithDescription("Adds to the target Lightroom collection by result (ok, no_target, refused, error)"),
		metric.WithUnit("{asset}"))
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return nil, err
	}
	return &publisher{
		lr: lr, emit: emit, maxDeliver: cfg.MaxDeliver, backoff: cfg.BackOff, now: time.Now,
		tracer: tracer, attempts: attempts, photos: photos, duration: duration, albumAdds: albumAdds,
	}, nil
}

// withAlbums turns on the album step (Adobe mode).
func (p *publisher) withAlbums(a albumAdder, t targetSource) *publisher {
	p.albums, p.targets = a, t
	return p
}

// handle is the natsx handler: nil → ack, Permanent → term, else nak with
// backoff.
func (p *publisher) handle(ctx context.Context, m natsx.Msg, ev events.PhotoDecided) error {
	stop := keepAlive(ctx, m, inProgressEvery)
	permanent, err := p.attempt(ctx, m.Delivered, ev)
	stop()
	if permanent {
		return natsx.Permanent(err)
	}
	return err
}

// inProgressEvery is well inside the 30 s AckWait.
const inProgressEvery = 10 * time.Second

// keepAlive tells JetStream every interval that m is still being worked on,
// until stop is called.
func keepAlive(ctx context.Context, m natsx.Msg, every time.Duration) (stop func()) {
	if m.Msg == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = m.InProgress()
			}
		}
	}()
	return func() { close(done) }
}

// attempt runs one delivery of photo.decided. It returns the error (nil on
// success) and whether the message must be terminated rather than retried.
func (p *publisher) attempt(ctx context.Context, delivered int, ev events.PhotoDecided) (permanent bool, err error) {
	assetID := lightroom.AssetID(ev.SHA256)
	ctx, span := p.tracer.Start(ctx, "publish", trace.WithAttributes(
		attribute.Int("keeper.attempt", delivered),
		attribute.String("keeper.photo.sha256", ev.SHA256),
		attribute.String("keeper.lightroom.asset_id", assetID),
	))
	start := p.now()
	defer func() {
		p.duration.Record(ctx, p.now().Sub(start).Seconds(),
			metric.WithAttributes(attribute.String("stage", "publish")))
		span.End()
	}()

	if !isSHA256(ev.SHA256) {
		err := fmt.Errorf("photo.decided with invalid sha256 %q", ev.SHA256)
		p.record(ctx, span, "failed", "invalid_event", err)
		return true, err // no photo.failed: there is no photo to attach it to
	}

	lrErr := p.toLightroom(ctx, assetID, ev)
	if lrErr == nil {
		if err := p.emit(ctx, events.SubjectPublished, events.MsgID(events.SubjectPublished, ev.SHA256),
			events.PhotoPublished{V: events.Version, SHA256: ev.SHA256, LightroomAssetID: assetID, Attempt: delivered},
		); err != nil {
			// Lightroom is done; only the event is missing. A redelivery
			// replays the idempotent steps and publishes again.
			p.record(ctx, span, "retry", "nats", err)
			return false, fmt.Errorf("publish %s: %w", events.SubjectPublished, err)
		}
		p.attempts.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "ok")))
		p.photos.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "published")))
		slog.InfoContext(ctx, "photo published to Lightroom",
			"sha256", ev.SHA256, "asset_id", assetID, "attempt", delivered, "rating", ev.Rating)
		return false, nil
	}

	class, retryable := classify(lrErr)
	if class == lightroom.ClassCanceled {
		// Shutting down: no photo.failed, the message comes back after restart.
		p.record(ctx, span, "retry", class, lrErr)
		return false, lrErr
	}
	last := p.maxDeliver > 0 && delivered >= p.maxDeliver // -1: unlimited
	failed := events.PhotoFailed{V: events.Version, SHA256: ev.SHA256, Attempt: delivered, ErrorClass: class,
		Error: failureText(lrErr)}
	if retryable && !last {
		at := p.now().Add(p.backoff[min(delivered, len(p.backoff))-1]).UTC()
		failed.RetryAt = &at
	}
	// One photo.failed per attempt: the attempt number is part of the dedup
	// ID, or JetStream would drop every retry's failure event.
	msgID := events.MsgID(events.SubjectFailed, ev.SHA256+":"+strconv.Itoa(delivered))
	if err := p.emit(ctx, events.SubjectFailed, msgID, failed); err != nil {
		slog.WarnContext(ctx, "could not publish photo.failed", "err", err)
	}

	if retryable && !last {
		p.record(ctx, span, "retry", class, lrErr)
		return false, lrErr
	}
	p.record(ctx, span, "failed", class, lrErr)
	p.photos.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "failed")))
	// Permanent error or last delivery: nothing redelivers this message.
	slog.ErrorContext(ctx, "giving up: photo not published to Lightroom",
		"sha256", ev.SHA256, "asset_id", assetID, "attempt", delivered, "max_deliver", p.maxDeliver,
		"error_class", class, "err", lrErr)
	return true, lrErr
}

// toLightroom runs the idempotent steps. A redelivery after a crash at any
// point replays them and ends in the same state.
func (p *publisher) toLightroom(ctx context.Context, assetID string, ev events.PhotoDecided) error {
	// 0. Adobe: know the collection before anything goes into the
	// catalogue. Until one is picked, the photo waits (retried every 30 s).
	var target lrlink.Target
	if p.targets != nil {
		t, ok := p.targets.Target()
		if !ok {
			p.albumAdds.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "no_target")))
			return errNoTarget
		}
		target = t
	}

	// The original as shot (JPEG or ARW raw); Lightroom gets the raw itself.
	path, err := spool.FindOriginal(ev.SHA256)
	if err != nil {
		return spoolError{err}
	}
	f, err := os.Open(path)
	if err != nil {
		return spoolError{err}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return spoolError{err}
	}

	// 1. Create, or get the existing asset (PUT on a sha-derived ID).
	name := ev.FileName // the camera's name; the spool name is only the sha
	if name == "" {
		name = info.Name()
	}
	asset, _, err := p.lr.CreateAsset(ctx, lightroom.NewAsset{
		ID: assetID, FileName: name, SHA256: ev.SHA256,
	})
	if err != nil {
		return err
	}

	// 2. Upload the original unless these exact bytes are already there.
	// The server also answers 200 for the same bytes, so this is only a
	// shortcut for replays, not what makes the step safe.
	if asset.MasterSHA256 != ev.SHA256 {
		mt, ok := media.TypeOf(path)
		if !ok {
			mt = media.JPEG
		}
		if _, err := p.lr.UploadMaster(ctx, assetID, string(mt), f, info.Size()); err != nil {
			return err
		}
	}

	// 3. Set the rating (a set, not an increment). The Adobe Partner API
	// cannot write one; the rating then stays in Keeper only.
	err = p.lr.SetRating(ctx, assetID, ev.Rating)
	written := err == nil
	if errors.Is(err, lightroom.ErrRatingUnsupported) {
		err = nil
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.Bool("keeper.lightroom.rating_written", written))
	if err != nil || p.albums == nil {
		return err
	}

	// 4. Add the asset to the chosen collection.
	return p.addToAlbum(ctx, assetID, target)
}

// addToAlbum is step 4 in its own span. Already in the collection counts
// as added (a replay); a 403 for the collection is permanent.
func (p *publisher) addToAlbum(ctx context.Context, assetID string, target lrlink.Target) error {
	ctx, span := p.tracer.Start(ctx, "lightroom.add_to_album", trace.WithAttributes(
		attribute.String("keeper.lightroom.album.id", target.AlbumID),
		attribute.String("keeper.lightroom.album.name", target.Name),
		attribute.String("keeper.lightroom.asset_id", assetID),
	))
	defer span.End()

	err := p.albums.AddToAlbum(ctx, target.AlbumID, []string{assetID})
	result := "ok"
	var refused *lightroom.AlbumRefusedError
	switch {
	case errors.As(err, &refused):
		result = "refused"
		err = albumRefusedError{name: target.Name, err: refused}
	case err != nil:
		result = "error"
	}
	span.SetAttributes(attribute.String("keeper.lightroom.album.result", result))
	p.albumAdds.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	if err != nil {
		class, _ := classify(err)
		span.SetAttributes(attribute.String("error.type", class))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// errNoTarget: Adobe mode, but nobody picked a collection in the console.
var errNoTarget = errors.New("no Lightroom collection chosen")

// albumRefusedError names the collection for the console.
type albumRefusedError struct {
	name string
	err  *lightroom.AlbumRefusedError
}

func (e albumRefusedError) Error() string {
	return "Lightroom refused collection " + e.name + ": " + e.err.Error()
}
func (e albumRefusedError) Unwrap() error { return e.err }

// failureText is the console's words for a failure, when the class alone
// would not tell the photographer what to do.
func failureText(err error) string {
	var refused albumRefusedError
	switch {
	case errors.Is(err, errNoTarget):
		return "No Lightroom collection chosen: pick one in the console"
	case errors.As(err, &refused):
		return "Lightroom refused collection " + refused.name
	case errors.Is(err, lightroom.ErrNotConnected):
		return "Lightroom not connected: connect it in the console"
	}
	if class, _ := lightroom.Classify(err); class == lightroom.ClassAuth {
		return "Lightroom sign-in expired: connect it again in the console"
	}
	return ""
}

type spoolError struct{ error }

func (e spoolError) Unwrap() error { return e.error }

func classify(err error) (class string, retryable bool) {
	var se spoolError
	if errors.As(err, &se) {
		return classSpool, true
	}
	if errors.Is(err, errNoTarget) {
		return lightroom.ClassNoTarget, true
	}
	return lightroom.Classify(err)
}

// record counts the attempt and marks the publish span as failed.
func (p *publisher) record(ctx context.Context, span trace.Span, result, class string, err error) {
	p.attempts.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	span.SetAttributes(attribute.String("error.type", class))
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
