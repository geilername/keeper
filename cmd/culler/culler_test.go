package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/telemetry"
)

type published struct {
	subject, msgID string
	v              any
}

func shaOf(name string) string {
	s := sha256.Sum256([]byte(name))
	return hex.EncodeToString(s[:])
}

// testCuller returns a culler whose publishes are recorded (or fail while
// *failing is true) and whose spans go to rec.
func testCuller(t *testing.T, failing *bool) (*culler, *[]published, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	var out []published
	c, err := newCuller(testCfg, 30*time.Second, func(_ context.Context, subject, msgID string, v any) error {
		if *failing {
			return errors.New("nats down")
		}
		out = append(out, published{subject, msgID, v})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, &out, rec
}

// heldPhoto builds a member as handle would: its parent is a consumer span
// in the photo's trace.
func heldPhoto(t *testing.T, name string, captured time.Duration, sharp float64, eyes string) *member {
	t.Helper()
	sha := shaOf(name)
	_, span := otel.Tracer("test").Start(telemetry.PhotoRootContext(context.Background(), sha), "process photo.scored")
	span.End()
	m := mem(sha, captured, hashA)
	m.frame.Features.Sharpness.Score = sharp
	m.frame.Features.Exposure.Score = 0.7
	m.frame.Features.Exposure.Mean = 0.5
	m.frame.Features.Eyes.State = eyes
	m.parent = span.SpanContext()
	return m
}

func TestBurstClosesOnIdleAndPublishes(t *testing.T) {
	failing := false
	c, out, rec := testCuller(t, &failing)
	ctx := context.Background()

	// Arrival order differs from capture order on purpose.
	frames := []*member{
		heldPhoto(t, "f3", 600*time.Millisecond, 0.83, events.EyesOpen),   // best
		heldPhoto(t, "f1", 0, 0.74, events.EyesOpen),                      // keeper
		heldPhoto(t, "f2", 300*time.Millisecond, 0.88, events.EyesClosed), // reject
		heldPhoto(t, "f4", 900*time.Millisecond, 0.21, events.EyesOpen),   // reject
		heldPhoto(t, "f5", 1200*time.Millisecond, 0.79, events.EyesOpen),  // keeper
		heldPhoto(t, "f6", 1500*time.Millisecond, 0.30, events.EyesOpen),  // reject
	}
	for _, m := range frames {
		c.w.add(m, wall0)
	}

	// Publishing fails: nothing is acked, the window stays open.
	failing = true
	c.tick(ctx, wall0.Add(testCfg.Idle))
	if len(c.w.open) != 1 || frames[0].msg.(*fakeMsg).acks != 0 {
		t.Fatal("a failed decision must leave the window open and unacked")
	}

	failing = false
	c.tick(ctx, wall0.Add(testCfg.Idle+time.Second))
	if len(c.w.open) != 0 || len(c.w.bySHA) != 0 {
		t.Fatal("window not closed")
	}
	for _, m := range frames {
		if acks := m.msg.(*fakeMsg).acks; acks != 1 {
			t.Errorf("%s acked %d times, want 1", m.frame.SHA256[:6], acks)
		}
	}

	// 1 burst.decided, then 6 photo.decided in capture order.
	if len(*out) != 7 {
		t.Fatalf("%d publishes, want 7", len(*out))
	}
	burst := (*out)[0].v.(events.BurstDecided)
	if burst.BurstID != shaOf("f1") || burst.Best != shaOf("f3") || burst.CloseReason != "idle" ||
		(*out)[0].msgID != "burst.decided:"+shaOf("f1") {
		t.Errorf("burst = %+v", burst)
	}
	buckets := map[string]int{}
	for i, p := range (*out)[1:] {
		d := p.v.(events.PhotoDecided)
		buckets[d.Bucket]++
		if want := shaOf("f" + string(rune('1'+i))); d.SHA256 != want || p.msgID != "photo.decided:"+want {
			t.Errorf("decision %d: %s (%s), want capture order", i, d.SHA256[:6], p.msgID)
		}
	}
	if buckets[events.BucketBest] != 1 || buckets[events.BucketKeeper] != 2 || buckets[events.BucketReject] != 3 {
		t.Errorf("buckets = %v, want 1 best, 2 keepers, 3 rejects", buckets)
	}

	checkSpans(t, rec, frames)
}

func checkSpans(t *testing.T, rec *tracetest.SpanRecorder, frames []*member) {
	t.Helper()
	parents := map[trace.SpanID]*member{}
	for _, m := range frames {
		parents[m.parent.SpanID()] = m
	}

	var burst sdktrace.ReadOnlySpan
	var decisions []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "burst.decide":
			if s.Status().Code == 0 { // the successful attempt
				burst = s
			}
		case "decision":
			decisions = append(decisions, s)
		}
	}
	if burst == nil {
		t.Fatal("no successful burst.decide span")
	}
	if burst.Parent().IsValid() {
		t.Error("burst.decide must start its own trace")
	}
	if len(burst.Links()) != len(frames) {
		t.Errorf("burst span has %d links, want %d", len(burst.Links()), len(frames))
	}
	linked := map[[2]string]bool{} // trace ID, span ID
	for _, l := range burst.Links() {
		linked[[2]string{l.SpanContext.TraceID().String(), l.SpanContext.SpanID().String()}] = true
	}
	for _, m := range frames {
		root := trace.SpanContextFromContext(telemetry.PhotoRootContext(context.Background(), m.frame.SHA256))
		if !linked[[2]string{root.TraceID().String(), root.SpanID().String()}] {
			t.Errorf("burst span has no link to the root of photo %s", m.frame.SHA256[:6])
		}
	}

	ok := 0
	for _, d := range decisions {
		if len(d.Links()) != 1 || d.Links()[0].SpanContext.SpanID() != burst.SpanContext().SpanID() {
			continue // decision of the failed first attempt
		}
		m := parents[d.Parent().SpanID()]
		if m == nil || d.SpanContext().TraceID() != m.parent.TraceID() {
			t.Errorf("decision span is not under its held delivery")
			continue
		}
		attrs := map[attribute.Key]attribute.Value{}
		for _, kv := range d.Attributes() {
			attrs[kv.Key] = kv.Value
		}
		if attrs["keeper.bucket"].AsString() == "" || attrs["keeper.rating"].AsInt64() == 0 {
			t.Errorf("decision span lacks bucket/rating: %v", d.Attributes())
		}
		ok++
	}
	if ok != len(frames) {
		t.Errorf("%d decision spans linked to the burst, want %d", ok, len(frames))
	}
}

func TestInProgressKeepsHeldMessagesAlive(t *testing.T) {
	failing := false
	c, _, _ := testCuller(t, &failing)
	m := mem(shaOf("a"), 0, hashA)
	c.w.add(m, wall0)
	c.lastProgress = wall0

	c.tick(context.Background(), wall0.Add(5*time.Second))
	c.tick(context.Background(), wall0.Add(10*time.Second)) // ackWait/3
	if got := m.msg.(*fakeMsg).progress; got != 1 {
		t.Errorf("InProgress sent %d times, want 1", got)
	}
}
