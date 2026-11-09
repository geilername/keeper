"""One photo through the stages decode -> faces -> sharpness -> exposure -> hash.

Each stage is an ordinary child span of the current span (the CONSUMER span
`process photo.ingested`). This runs in a worker thread via asyncio.to_thread,
which copies contextvars, so the OTel context and therefore the parent span
come along. No process pool, so no span-context rebuilding is needed (§5.1).
"""

from __future__ import annotations

import time
from contextlib import contextmanager
from pathlib import Path
from typing import Any

from opentelemetry import trace

from . import features

tracer = trace.get_tracer("keeper.scorer")


@contextmanager
def _stage(name: str, timings: dict[str, float]):
    """A child span per stage, plus the wall time in ms for the event payload."""
    with tracer.start_as_current_span(name) as span:
        t0 = time.perf_counter()
        yield span
        timings[name] = round((time.perf_counter() - t0) * 1000.0, 1)


def analyse(path: Path, detector: features.EyeDetector) -> tuple[dict[str, Any], dict[str, float]]:
    """Return (features, timings_ms) in the photo.scored shape.

    Raises FileNotFoundError / features.DecodeError for files that will never
    score (the caller terminates those), anything else is treated as transient.
    """
    timings: dict[str, float] = {}

    with _stage("decode", timings) as span:
        data = path.read_bytes()
        gray = features.decode(data)
        span.set_attributes(
            {
                "keeper.image.size_bytes": len(data),
                "keeper.image.work_width": int(gray.shape[1]),
                "keeper.image.work_height": int(gray.shape[0]),
            }
        )

    with _stage("faces", timings) as span:
        eyes = detector.detect(gray)
        span.set_attributes({"keeper.eyes": eyes["state"], "keeper.faces": eyes["faces"]})

    with _stage("sharpness", timings) as span:
        sharpness = features.sharpness(gray)
        span.set_attribute("keeper.sharpness.score", sharpness["score"])

    with _stage("exposure", timings) as span:
        exposure = features.exposure(gray)
        span.set_attribute("keeper.exposure.score", exposure["score"])

    with _stage("hash", timings):
        dhash = features.dhash(gray)

    result = {"sharpness": sharpness, "exposure": exposure, "eyes": eyes, "dhash": dhash}
    return result, timings
