"""NATS JetStream loop: photo.ingested -> analyse -> photo.scored.

Stateless. Retries come from JetStream redelivery (ADR-1): ack on success,
nak with a delay on a transient failure, term on a photo that can never score.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import signal
import time
from pathlib import Path

import nats
import nats.errors
from nats.aio.msg import Msg
from nats.js import JetStreamContext
from nats.js.api import AckPolicy, ConsumerConfig, DeliverPolicy, StorageType, StreamConfig
from nats.js.errors import BadRequestError, NotFoundError
from opentelemetry import metrics, propagate, trace
from opentelemetry.trace import SpanKind, Status, StatusCode

from . import features, pipeline, telemetry

log = logging.getLogger("keeper.scorer")
tracer = trace.get_tracer("keeper.scorer")

STREAM = "PHOTOS"
STREAM_SUBJECTS = ["photo.>", "burst.>"]
IN_SUBJECT = "photo.ingested"
OUT_SUBJECT = "photo.scored"
DURABLE = "scorer"

ACK_WAIT_S = 30.0  # one photo takes well under 1 s; this only guards a hung scorer
MAX_DELIVER = 6
MAX_NAK_DELAY_S = 30.0
STREAM_IN_USE = 10058  # JetStream err_code: stream name already in use (with another config)

SHA256 = re.compile(r"^[0-9a-f]{64}$")


class Permanent(Exception):
    """Redelivery cannot fix this message: terminate it."""


def nak_delay(num_delivered: int) -> float:
    """Exponential backoff for redelivery: 2, 4, 8, 16, 30, 30 ... seconds."""
    return min(MAX_NAK_DELAY_S, 2.0 ** max(1, num_delivered))


def parse_ingested(data: bytes, spool: Path) -> dict:
    """Validate a photo.ingested payload. Raises Permanent if it is unusable."""
    try:
        event = json.loads(data)
    except ValueError as e:
        raise Permanent(f"payload is not JSON: {e}") from e
    if not isinstance(event, dict):
        raise Permanent("payload is not a JSON object")
    if event.get("v") != 1:
        raise Permanent(f"unsupported photo.ingested version {event.get('v')!r}")
    if not isinstance(event.get("sha256"), str) or not SHA256.match(event["sha256"]):
        raise Permanent("sha256 missing or malformed")
    if not isinstance(event.get("path"), str) or not isinstance(event.get("captured_at"), str):
        raise Permanent("path or captured_at missing")
    # Only ever read inside the spool volume, whatever the message says.
    path = Path(event["path"]).resolve()
    if not path.is_relative_to(spool):
        raise Permanent(f"path {event['path']!r} is outside {spool}")
    event["path"] = path
    return event


def scored_event(ingested: dict, feats: dict, timings_ms: dict) -> dict:
    return {
        "v": 1,
        "sha256": ingested["sha256"],
        "file_name": ingested.get("file_name", ""),  # passed through for Lightroom
        "captured_at": ingested["captured_at"],
        "features": feats,
        "timings_ms": timings_ms,
    }


class Scorer:
    def __init__(self, js: JetStreamContext, detector: features.EyeDetector, spool: Path) -> None:
        self.js = js
        self.detector = detector
        self.spool = spool
        # Bucket boundaries in seconds; the SDK defaults are tuned for ms.
        self.duration = metrics.get_meter("keeper.scorer").create_histogram(
            "keeper.stage.duration",
            unit="s",
            description="Time spent per pipeline stage",
            explicit_bucket_boundaries_advisory=[0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10],
        )

    async def handle(self, msg: Msg) -> None:
        """Process one message under a CONSUMER span that continues the photo trace."""
        md = msg.metadata
        headers = msg.headers or {}
        parent = propagate.extract(headers)
        attrs = {
            "messaging.system": "nats",
            "messaging.destination.name": msg.subject,
            "messaging.operation.type": "process",
            "messaging.consumer.group.name": DURABLE,
            "messaging.message.id": headers.get("Nats-Msg-Id", str(md.sequence.stream)),
            "messaging.nats.message.delivery_count": md.num_delivered,
        }
        start = time.perf_counter()
        with tracer.start_as_current_span(
            f"process {IN_SUBJECT}",
            context=parent,
            kind=SpanKind.CONSUMER,
            attributes=attrs,
            record_exception=False,  # we record and classify errors ourselves
            set_status_on_exception=False,
        ) as span:
            await self._process(msg, span, md.num_delivered)
        self.duration.record(time.perf_counter() - start, {"stage": "score"})

    async def _process(self, msg: Msg, span: trace.Span, delivered: int) -> None:
        sha = None
        try:
            event = parse_ingested(msg.data, self.spool)
            sha = event["sha256"]
            span.set_attribute("keeper.photo.sha256", sha)
            try:
                feats, timings = await asyncio.to_thread(pipeline.analyse, event["path"], self.detector)
            except (FileNotFoundError, features.DecodeError) as e:
                raise Permanent(f"{type(e).__name__}: {e}") from e
            await self._publish(scored_event(event, feats, timings), sha)
            await msg.ack()
            span.set_attribute("keeper.outcome", "scored")
            log.info(
                "photo scored",
                extra={
                    "sha256": sha,
                    "eyes": feats["eyes"]["state"],
                    "sharpness": feats["sharpness"]["score"],
                    "exposure": feats["exposure"]["score"],
                },
            )
        except Permanent as e:
            span.record_exception(e)
            span.set_status(Status(StatusCode.ERROR, str(e)))
            span.set_attribute("keeper.outcome", "terminated")
            await msg.term()
            log.error("photo terminated, will not retry: %s", e, extra={"sha256": sha})
        except Exception as e:  # noqa: BLE001 - anything else is worth a retry
            span.record_exception(e)
            span.set_status(Status(StatusCode.ERROR, str(e)))
            delay = nak_delay(delivered)
            span.set_attribute("keeper.outcome", "retry")
            await msg.nak(delay=delay)
            level = logging.ERROR if delivered >= MAX_DELIVER else logging.WARNING
            log.log(
                level,
                "scoring failed (delivery %d/%d), retry in %.0fs: %s",
                delivered,
                MAX_DELIVER,
                delay,
                e,
                extra={"sha256": sha},
            )

    async def _publish(self, event: dict, sha: str) -> None:
        msg_id = f"{OUT_SUBJECT}:{sha}"  # JetStream drops duplicates inside its window (ADR-2)
        headers = {"Nats-Msg-Id": msg_id}
        with tracer.start_as_current_span(
            f"publish {OUT_SUBJECT}",
            kind=SpanKind.PRODUCER,
            attributes={
                "messaging.system": "nats",
                "messaging.destination.name": OUT_SUBJECT,
                "messaging.operation.type": "send",
                "messaging.message.id": msg_id,
            },
        ) as span:
            propagate.inject(headers)  # traceparent of this PRODUCER span
            ack = await self.js.publish(OUT_SUBJECT, json.dumps(event).encode(), headers=headers)
            span.set_attribute("keeper.duplicate", bool(ack.duplicate))
            if ack.duplicate:
                log.info("photo.scored already published, JetStream dropped the duplicate", extra={"sha256": sha})


async def ensure_stream(js: JetStreamContext) -> None:
    """Create the PHOTOS stream if it is missing. Never changes an existing one."""
    try:
        await js.stream_info(STREAM)
        return
    except NotFoundError:
        pass
    try:
        await js.add_stream(StreamConfig(name=STREAM, subjects=STREAM_SUBJECTS, storage=StorageType.FILE))
        log.info("created stream %s", STREAM)
    except BadRequestError as e:
        if e.err_code != STREAM_IN_USE:  # another service won the race
            raise


async def connect(url: str) -> nats.NATS:
    """Connect, retrying until NATS is up (compose start order is not readiness)."""
    delay = 1.0
    while True:
        try:
            return await nats.connect(url, name="scorer", max_reconnect_attempts=-1)
        except (nats.errors.NoServersError, OSError, TimeoutError) as e:
            log.warning("NATS not reachable at %s (%s), retrying in %.0fs", url, e, delay)
            await asyncio.sleep(delay)
            delay = min(delay * 2, 10.0)


async def run() -> None:
    url = os.environ.get("NATS_URL", "nats://nats:4222")
    spool = Path(os.environ.get("SPOOL_DIR", "/spool")).resolve()
    detector = features.EyeDetector.load()

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, stop.set)

    nc = await connect(url)
    js = nc.jetstream()
    await ensure_stream(js)
    sub = await js.pull_subscribe(
        IN_SUBJECT,
        durable=DURABLE,
        stream=STREAM,
        config=ConsumerConfig(
            durable_name=DURABLE,
            filter_subject=IN_SUBJECT,
            deliver_policy=DeliverPolicy.ALL,
            ack_policy=AckPolicy.EXPLICIT,
            ack_wait=ACK_WAIT_S,
            max_deliver=MAX_DELIVER,
        ),
    )
    scorer = Scorer(js, detector, spool)
    log.info("scorer consuming %s as durable %r from %s", IN_SUBJECT, DURABLE, url)

    # One photo at a time: each takes well under a second, and a backlog
    # simply waits in JetStream. Stop is checked between messages.
    while not stop.is_set():
        try:
            msgs = await sub.fetch(batch=1, timeout=2)
        except (nats.errors.TimeoutError, asyncio.TimeoutError):
            continue
        for msg in msgs:
            await scorer.handle(msg)

    log.info("shutting down")
    await nc.drain()


def main() -> None:
    shutdown = telemetry.setup()
    try:
        asyncio.run(run())
    finally:
        shutdown()


if __name__ == "__main__":
    main()
