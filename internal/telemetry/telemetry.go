// Package telemetry wires OpenTelemetry traces, metrics and logs for every
// Keeper Go service. Services only need OTEL_EXPORTER_OTLP_ENDPOINT; the
// collector owns everything vendor-specific (ADR-5).
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Version is stamped at build time via -ldflags.
var Version = "dev"

// Shutdown flushes and stops all providers.
type Shutdown func(context.Context) error

// Setup installs global tracer, meter and logger providers plus a default
// slog logger that writes to stdout and to OTLP. Call the returned Shutdown
// before exit so buffered telemetry is flushed.
func Setup(ctx context.Context, service string) (Shutdown, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceName(service),
			semconv.ServiceNamespace("keeper"),
			semconv.ServiceVersion(Version),
		),
	)
	if err != nil {
		return nil, err
	}
	// OTEL_RESOURCE_ATTRIBUTES / OTEL_SERVICE_NAME still win when set.
	if envRes, err := resource.New(ctx, resource.WithFromEnv(), resource.WithHost()); err == nil {
		if merged, err := resource.Merge(res, envRes); err == nil {
			res = merged
		}
	}

	var shutdowns []func(context.Context) error
	shutdown := func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			errs = append(errs, shutdowns[i](ctx))
		}
		return errors.Join(errs...)
	}
	fail := func(err error) (Shutdown, error) {
		_ = shutdown(ctx)
		return nil, err
	}

	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return fail(err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp, sdktrace.WithBatchTimeout(time.Second)),
	)
	shutdowns = append(shutdowns, tp.Shutdown)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return fail(err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(10*time.Second))),
	)
	shutdowns = append(shutdowns, mp.Shutdown)
	otel.SetMeterProvider(mp)

	logExp, err := otlploghttp.New(ctx)
	if err != nil {
		return fail(err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)
	shutdowns = append(shutdowns, lp.Shutdown)
	global.SetLoggerProvider(lp)

	// Logs go out as OTLP (with trace/span IDs from ctx) and stay readable
	// on stdout for `docker compose logs`.
	stdout := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(slog.NewMultiHandler(
		stdout,
		otelslog.NewHandler(service, otelslog.WithLoggerProvider(lp)),
	)))

	// Export errors go to stdout only, never back into the OTLP pipeline
	// that is failing.
	stdoutLog := slog.New(stdout)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		stdoutLog.Warn("otel export error", "err", err)
	}))

	return shutdown, nil
}
