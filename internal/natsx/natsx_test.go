package natsx

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/geilername/keeper/internal/events"
)

// Integration test: NATS_URL=nats://127.0.0.1:4222 go test ./internal/natsx/
func TestPublishConsumeTraceAndRetry(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set")
	}
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nc, js, err := Connect(ctx, url, "natsx-test")
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	sha := "ab" + time.Now().Format("150405.000000000")
	subject := "photo.test"
	_ = js.DeleteConsumer(ctx, events.Stream, "natsx-test")

	ctx, parent := otel.Tracer("t").Start(ctx, "parent")
	in := events.PhotoIngested{V: 1, SHA256: sha}
	if err := Publish(ctx, js, subject, events.MsgID(subject, sha), in); err != nil {
		t.Fatal(err)
	}
	// Same Nats-Msg-Id again: JetStream must drop it.
	if err := Publish(ctx, js, subject, events.MsgID(subject, sha), in); err != nil {
		t.Fatal(err)
	}
	parent.End()

	type result struct {
		delivered int
		trace     trace.TraceID
	}
	got := make(chan result, 4)
	cctx, ccancel := context.WithCancel(ctx)
	go Consume(cctx, js, ConsumerConfig{
		Durable: "natsx-test", FilterSubject: subject,
		BackOff: []time.Duration{100 * time.Millisecond},
	}, func(p events.PhotoIngested) string { return p.SHA256 },
		func(ctx context.Context, m Msg, p events.PhotoIngested) error {
			if p.SHA256 != sha {
				return nil // leftovers from earlier runs
			}
			got <- result{m.Delivered, trace.SpanContextFromContext(ctx).TraceID()}
			if m.Delivered == 1 {
				return errors.New("transient")
			}
			return nil
		})

	first, second := <-got, <-got
	ccancel()
	if first.delivered != 1 || second.delivered != 2 {
		t.Fatalf("deliveries = %d,%d; want 1,2", first.delivered, second.delivered)
	}
	if first.trace != parent.SpanContext().TraceID() || second.trace != first.trace {
		t.Fatal("consumer spans are not in the producer's trace")
	}
	select {
	case r := <-got:
		t.Fatalf("unexpected extra delivery %+v (dedup failed?)", r)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestHeaderCarrierInteropWithPython(t *testing.T) {
	h := nats.Header{}
	headerCarrier(h).Set("traceparent", "00-abc-def-01")
	if _, ok := h["traceparent"]; !ok {
		t.Fatalf("written key not lowercase: %v", h)
	}
	py := nats.Header{"traceparent": {"x"}} // as nats-py sends it
	if headerCarrier(py).Get("traceparent") != "x" {
		t.Fatal("lowercase key not read")
	}
	canon := nats.Header{"Traceparent": {"y"}}
	if headerCarrier(canon).Get("traceparent") != "y" {
		t.Fatal("canonical key not read")
	}
}
