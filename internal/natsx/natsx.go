// Package natsx is the one place where Keeper services publish to and
// consume from JetStream. It carries W3C trace context in message headers,
// creates PRODUCER/CONSUMER spans with messaging semconv attributes, and
// maps handler results to ack / nak-with-backoff / term (ADR-1).
package natsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/telemetry"
)

const tracerName = "github.com/geilername/keeper/internal/natsx"

// Attribute for the delivery count; there is no semconv name for it. The
// Python scorer uses the same key.
const attrDeliveryCount = "messaging.nats.message.delivery_count"

// Connect dials NATS (retrying until ctx ends) and returns a JetStream handle
// with the PHOTOS stream in place.
func Connect(ctx context.Context, url, service string) (*nats.Conn, jetstream.JetStream, error) {
	nc, err := nats.Connect(url,
		nats.Name(service),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	for {
		err = EnsureStream(ctx, js)
		if err == nil {
			return nc, js, nil
		}
		slog.WarnContext(ctx, "waiting for JetStream", "err", err)
		select {
		case <-ctx.Done():
			nc.Close()
			return nil, nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// EnsureStream creates the PHOTOS stream if it is missing and never changes
// an existing one (the Python scorer does the same).
func EnsureStream(ctx context.Context, js jetstream.JetStream) error {
	if _, err := js.Stream(ctx, events.Stream); err == nil {
		return nil
	} else if !errors.Is(err, jetstream.ErrStreamNotFound) {
		return err
	}
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     events.Stream,
		Subjects: events.StreamSubjects,
		Storage:  jetstream.FileStorage,
	})
	if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
		return nil // another service won the race
	}
	return err
}

// Publish sends v as JSON on subject with Nats-Msg-Id = msgID (dedup,
// ADR-2) inside a PRODUCER span whose context travels in the headers.
func Publish(ctx context.Context, js jetstream.JetStream, subject, msgID string, v any) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "publish "+subject,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.operation.type", "send"),
			attribute.String("messaging.destination.name", subject),
			attribute.String("messaging.message.id", msgID),
		))
	defer span.End()

	data, err := json.Marshal(v)
	if err != nil {
		return fail(span, err)
	}
	msg := nats.NewMsg(subject)
	msg.Data = data
	msg.Header.Set(jetstream.MsgIDHeader, msgID)
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier(msg.Header))

	ack, err := js.PublishMsg(ctx, msg)
	if err != nil {
		return fail(span, err)
	}
	span.SetAttributes(attribute.Bool("messaging.nats.duplicate", ack.Duplicate))
	if ack.Duplicate {
		slog.InfoContext(ctx, "duplicate publish dropped by JetStream", "subject", subject, "msg_id", msgID)
	}
	return nil
}

// Handler errors steer what happens to the message.
var (
	// ErrHold leaves the message unacked: the handler acks it later (the
	// culler holds messages while a burst window is open, ADR-4).
	ErrHold = errors.New("natsx: hold message")
)

// Permanent wraps an error that redelivery cannot fix; the message is
// terminated instead of retried.
func Permanent(err error) error { return permanentError{err} }

type permanentError struct{ error }

func (p permanentError) Unwrap() error { return p.error }

// Msg is what a handler gets: the raw message plus its delivery count.
type Msg struct {
	jetstream.Msg
	Delivered int
}

// ConsumerConfig is the subset of JetStream consumer settings services pick.
type ConsumerConfig struct {
	Durable       string
	FilterSubject string
	AckWait       time.Duration   // default 30s
	MaxDeliver    int             // default 8
	BackOff       []time.Duration // nak delays by delivery count; default 2s,5s,15s,30s
	MaxAckPending int             // default 1000
}

func (c *ConsumerConfig) defaults() {
	if c.AckWait == 0 {
		c.AckWait = 30 * time.Second
	}
	if c.MaxDeliver == 0 {
		c.MaxDeliver = 8
	}
	if len(c.BackOff) == 0 {
		c.BackOff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}
	}
	if c.MaxAckPending == 0 {
		c.MaxAckPending = 1000
	}
}

// Consume runs handler for every message of a durable consumer until ctx
// ends. Each message is processed inside a CONSUMER span that continues the
// producer's trace (or, without a traceparent, the photo trace derived from
// sha256 when sha is non-empty).
//
// handler result: nil → ack; ErrHold → nothing; Permanent → term;
// anything else → nak with the BackOff delay for this delivery.
func Consume[T any](ctx context.Context, js jetstream.JetStream, cfg ConsumerConfig,
	sha func(T) string, handler func(context.Context, Msg, T) error,
) error {
	cfg.defaults()
	cons, err := js.CreateOrUpdateConsumer(ctx, events.Stream, jetstream.ConsumerConfig{
		Durable:       cfg.Durable,
		FilterSubject: cfg.FilterSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxDeliver:    cfg.MaxDeliver,
		MaxAckPending: cfg.MaxAckPending,
	})
	if err != nil {
		return fmt.Errorf("consumer %s: %w", cfg.Durable, err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) { handle(ctx, cfg, m, sha, handler) })
	if err != nil {
		return err
	}
	<-ctx.Done()
	cc.Drain()
	<-cc.Closed()
	return nil
}

func handle[T any](ctx context.Context, cfg ConsumerConfig, m jetstream.Msg,
	sha func(T) string, handler func(context.Context, Msg, T) error,
) {
	delivered := 1
	if md, err := m.Metadata(); err == nil {
		delivered = int(md.NumDelivered)
	}

	var payload T
	decodeErr := json.Unmarshal(m.Data(), &payload)

	parent := otel.GetTextMapPropagator().Extract(ctx, headerCarrier(m.Headers()))
	if !trace.SpanContextFromContext(parent).IsValid() && decodeErr == nil && sha != nil {
		parent = telemetry.PhotoRootContext(ctx, sha(payload))
	}
	msgID := m.Headers().Get(jetstream.MsgIDHeader)
	ctx, span := otel.Tracer(tracerName).Start(parent, "process "+m.Subject(),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.operation.type", "process"),
			attribute.String("messaging.destination.name", m.Subject()),
			attribute.String("messaging.consumer.group.name", cfg.Durable),
			attribute.String("messaging.message.id", msgID),
			attribute.Int(attrDeliveryCount, delivered),
		))
	defer span.End()

	err := decodeErr
	if err != nil {
		err = Permanent(fmt.Errorf("decode %s: %w", m.Subject(), err))
	} else {
		err = handler(ctx, Msg{Msg: m, Delivered: delivered}, payload)
	}

	var perm permanentError
	switch {
	case err == nil:
		if aerr := m.Ack(); aerr != nil {
			slog.WarnContext(ctx, "ack failed", "err", aerr)
		}
	case errors.Is(err, ErrHold):
		span.SetAttributes(attribute.Bool("keeper.held", true))
	case errors.As(err, &perm):
		_ = fail(span, err)
		slog.ErrorContext(ctx, "message terminated", "subject", m.Subject(), "err", err)
		_ = m.TermWithReason(err.Error())
	default:
		_ = fail(span, err)
		delay := cfg.BackOff[min(delivered, len(cfg.BackOff))-1]
		span.SetAttributes(attribute.Float64("keeper.retry_in_s", delay.Seconds()))
		slog.WarnContext(ctx, "processing failed, will retry",
			"subject", m.Subject(), "delivery", delivered, "max", cfg.MaxDeliver,
			"retry_in", delay, "err", err)
		_ = m.NakWithDelay(delay)
	}
}

func fail(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return err
}
