package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const sha = "00112233445566778899aabbccddeeff0123456789abcdef0011223344556677"

func TestPhotoRootGetsDerivedIDs(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithIDGenerator(newIDGenerator()))
	tr := tp.Tracer("test")

	ctx, root := tr.Start(WithPhotoRoot(context.Background(), sha), "photo x.jpg", trace.WithNewRoot())
	_, child := tr.Start(ctx, "child")
	child.End()
	root.End()

	// A later service without a traceparent still lands in the same trace.
	_, late := tr.Start(PhotoRootContext(context.Background(), sha), "late")
	late.End()

	spans := rec.Ended()
	if got := spans[1].SpanContext().TraceID().String(); got != sha[:32] {
		t.Fatalf("root trace id = %s, want %s", got, sha[:32])
	}
	if got := spans[1].SpanContext().SpanID().String(); got != sha[32:48] {
		t.Fatalf("root span id = %s, want %s", got, sha[32:48])
	}
	if spans[0].SpanContext().SpanID() == spans[1].SpanContext().SpanID() {
		t.Fatal("child reused the root span id")
	}
	if spans[2].SpanContext().TraceID() != spans[1].SpanContext().TraceID() ||
		spans[2].Parent().SpanID() != spans[1].SpanContext().SpanID() {
		t.Fatal("late span is not a child of the photo root")
	}
}
