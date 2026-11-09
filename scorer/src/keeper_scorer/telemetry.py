"""OpenTelemetry setup for the scorer: traces, metrics and logs over OTLP/HTTP.

Mirrors internal/telemetry in Go. The exporters read OTEL_EXPORTER_OTLP_ENDPOINT
(e.g. http://otel-collector:4318) themselves; the collector owns everything
vendor-specific (ADR-5).
"""

from __future__ import annotations

import logging
import sys
from collections.abc import Callable

from opentelemetry import metrics, propagate, trace
from opentelemetry.baggage.propagation import W3CBaggagePropagator
from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.propagators.composite import CompositePropagator
from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from . import __version__

SERVICE = "scorer"


class _TraceIdFilter(logging.Filter):
    """Adds the current trace/span ID to stdout lines so they match Dash0."""

    def filter(self, record: logging.LogRecord) -> bool:
        ctx = trace.get_current_span().get_span_context()
        record.trace = f" trace_id={ctx.trace_id:032x} span_id={ctx.span_id:016x}" if ctx.is_valid else ""
        return True


class _NotOtel(logging.Filter):
    """Keep the exporters' own error logs out of the OTLP pipeline that is failing."""

    def filter(self, record: logging.LogRecord) -> bool:
        return not record.name.startswith("opentelemetry")


def setup(level: int = logging.INFO) -> Callable[[], None]:
    """Install global providers and logging. Returns a shutdown function that flushes."""
    # OTEL_RESOURCE_ATTRIBUTES / OTEL_SERVICE_NAME are merged in by Resource.create.
    resource = Resource.create(
        {
            "service.name": SERVICE,
            "service.namespace": "keeper",
            "service.version": __version__,
        }
    )

    tp = TracerProvider(resource=resource)
    tp.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(), schedule_delay_millis=1000))
    trace.set_tracer_provider(tp)
    # W3C traceparent (+ baggage), same as the Go services.
    propagate.set_global_textmap(CompositePropagator([TraceContextTextMapPropagator(), W3CBaggagePropagator()]))

    mp = MeterProvider(
        resource=resource,
        metric_readers=[PeriodicExportingMetricReader(OTLPMetricExporter(), export_interval_millis=10_000)],
    )
    metrics.set_meter_provider(mp)

    lp = LoggerProvider(resource=resource)
    lp.add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter()))

    stdout = logging.StreamHandler(sys.stdout)
    stdout.addFilter(_TraceIdFilter())
    stdout.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s%(trace)s"))

    # LoggingHandler turns stdlib records into OTLP log records and attaches
    # the trace/span ID of the active span, so logs correlate with traces.
    otlp = LoggingHandler(level=level, logger_provider=lp)
    otlp.addFilter(_NotOtel())

    root = logging.getLogger()
    root.setLevel(level)
    root.handlers[:] = [stdout, otlp]
    logging.getLogger("nats").setLevel(logging.WARNING)

    def shutdown() -> None:
        # Flush in reverse order of creation; never raise on the way out.
        for provider in (lp, mp, tp):
            try:
                provider.shutdown()
            except Exception:  # noqa: BLE001
                pass

    return shutdown
