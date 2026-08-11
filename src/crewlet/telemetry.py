"""OpenTelemetry setup and helpers for Crewlet.

Provides a single ``init_telemetry()`` entry point called at engine
startup and helper functions for extracting trace context from the
current OTel span into Event fields.
"""

from __future__ import annotations

from opentelemetry import trace
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.trace import (
    StatusCode,
    format_span_id,
    format_trace_id,
)

from crewlet._logging import get_logger

logger = get_logger("telemetry")

_SERVICE_NAME = "crewlet"
_initialized = False

# Set a default SDK TracerProvider so spans are created even before
# init_telemetry() is called (e.g. in tests).  init_telemetry()
# replaces this with a fully configured provider if needed.
if not isinstance(trace.get_tracer_provider(), TracerProvider):
    trace.set_tracer_provider(TracerProvider())

tracer = trace.get_tracer(_SERVICE_NAME)


def init_telemetry(
    *,
    otlp_endpoint: str = "",
    service_name: str = _SERVICE_NAME,
) -> None:
    """Initialize the OTel TracerProvider with an OTLP exporter.

    Call once at engine startup.  If *otlp_endpoint* is empty, a
    console-only provider is configured (spans are still created but
    not exported).
    """
    global _initialized  # noqa: PLW0603
    if _initialized:
        return

    resource = Resource.create({"service.name": service_name})

    provider = TracerProvider(resource=resource)

    if otlp_endpoint:
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import (
            OTLPSpanExporter,
        )
        from opentelemetry.sdk.trace.export import BatchSpanProcessor

        exporter = OTLPSpanExporter(endpoint=otlp_endpoint)
        provider.add_span_processor(BatchSpanProcessor(exporter))
        logger.info(
            "otlp_exporter_configured",
            endpoint=otlp_endpoint,
        )

    trace.set_tracer_provider(provider)
    _initialized = True
    logger.info("telemetry_initialized", service=service_name)


def shutdown_telemetry() -> None:
    """Flush and shut down the tracer provider."""
    provider = trace.get_tracer_provider()
    if hasattr(provider, "shutdown"):
        provider.shutdown()
    logger.info("telemetry_shutdown")


def current_trace_id() -> str:
    """Return the current span's trace-id as a 32-char hex string, or empty."""
    span = trace.get_current_span()
    ctx = span.get_span_context()
    if ctx and ctx.trace_id:
        return format_trace_id(ctx.trace_id)
    return ""


def current_span_id() -> str:
    """Return the current span's span-id as a 16-char hex string, or empty."""
    span = trace.get_current_span()
    ctx = span.get_span_context()
    if ctx and ctx.span_id:
        return format_span_id(ctx.span_id)
    return ""


def current_parent_span_id() -> str:
    """Return the parent span-id if available, or empty.

    OTel manages parent context automatically — this extracts it from
    the current span for writing into Event fields.
    """
    span = trace.get_current_span()
    if hasattr(span, "parent") and span.parent:
        return format_span_id(span.parent.span_id)
    return ""


def set_span_error(error: Exception) -> None:
    """Record an error on the current span."""
    span = trace.get_current_span()
    span.set_status(StatusCode.ERROR, str(error))
    span.record_exception(error)


def restore_context(trace_id_hex: str, span_id_hex: str) -> trace.Context | None:
    """Restore an OTel context from hex trace/span IDs.

    Use this to reconnect the trace when crossing async boundaries
    (e.g. event queue → handler).  Pass the returned context to
    ``tracer.start_as_current_span(context=...)``.
    """
    if not trace_id_hex or not span_id_hex:
        return None
    try:
        from opentelemetry.trace import (
            NonRecordingSpan,
            SpanContext,
            TraceFlags,
        )

        span_context = SpanContext(
            trace_id=int(trace_id_hex, 16),
            span_id=int(span_id_hex, 16),
            is_remote=True,
            trace_flags=TraceFlags(TraceFlags.SAMPLED),
        )
        parent_span = NonRecordingSpan(span_context)
        return trace.set_span_in_context(parent_span)
    except (ValueError, OverflowError):
        return None
