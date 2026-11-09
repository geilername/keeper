package telemetry

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"sync"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// One trace per photo (ADR-6): the trace ID and the root span ID are derived
// from the photo's sha256, so every service, restart and redelivery lands in
// the same waterfall without coordination.
//
//	trace_id = sha256[0:16]   (bytes)
//	root     = sha256[16:24]  (span ID of the "photo <file>" root span)

// PhotoIDs returns the trace ID and root span ID for a hex sha256.
func PhotoIDs(sha256Hex string) (trace.TraceID, trace.SpanID, error) {
	var tid trace.TraceID
	var sid trace.SpanID
	b, err := hex.DecodeString(sha256Hex)
	if err != nil || len(b) != 32 {
		return tid, sid, fmt.Errorf("telemetry: not a sha256: %q", sha256Hex)
	}
	copy(tid[:], b[0:16])
	copy(sid[:], b[16:24])
	return tid, sid, nil
}

// PhotoRootContext returns ctx with the photo's root span as remote parent.
// Use it when a message arrives without a traceparent header: new spans
// still join the photo's trace, under its root.
func PhotoRootContext(ctx context.Context, sha256Hex string) context.Context {
	tid, sid, err := PhotoIDs(sha256Hex)
	if err != nil {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true,
	}))
}

type forcedIDsKey struct{}

type forcedIDs struct {
	tid trace.TraceID
	sid trace.SpanID
}

// WithPhotoRoot marks ctx so the next new-root span started from it gets the
// photo's derived trace and span IDs. Start it with trace.WithNewRoot().
func WithPhotoRoot(ctx context.Context, sha256Hex string) context.Context {
	tid, sid, err := PhotoIDs(sha256Hex)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, forcedIDsKey{}, forcedIDs{tid, sid})
}

// idGenerator is the SDK's random generator, except that a root span
// started from a WithPhotoRoot context gets the derived IDs.
type idGenerator struct {
	mu  sync.Mutex
	rnd *rand.ChaCha8
}

var _ sdktrace.IDGenerator = (*idGenerator)(nil)

func newIDGenerator() *idGenerator {
	var seed [32]byte
	_, _ = crand.Read(seed[:])
	return &idGenerator{rnd: rand.NewChaCha8(seed)}
}

func (g *idGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if f, ok := ctx.Value(forcedIDsKey{}).(forcedIDs); ok {
		return f.tid, f.sid
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var tid trace.TraceID
	var sid trace.SpanID
	for !tid.IsValid() {
		_, _ = g.rnd.Read(tid[:])
	}
	for !sid.IsValid() {
		_, _ = g.rnd.Read(sid[:])
	}
	return tid, sid
}

func (g *idGenerator) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	g.mu.Lock()
	defer g.mu.Unlock()
	var sid trace.SpanID
	for !sid.IsValid() {
		_, _ = g.rnd.Read(sid[:])
	}
	return sid
}
