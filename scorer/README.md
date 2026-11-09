# Keeper scorer

Stateless Python service: consumes `photo.ingested` from JetStream, computes
image features and publishes `photo.scored`. One photo at a time, ~100 ms for
a 24 MP JPEG.

```
photo.ingested ─► process photo.ingested (CONSUMER span, child of the incoming traceparent)
                    ├─ decode      read /spool file, grayscale, long edge ≤ 1024 px
                    ├─ faces       Haar face + eye cascades → eyes open | closed | no_face
                    ├─ sharpness   variance of Laplacian → score v/(v+100)
                    ├─ exposure    mean luminance, clipped fractions → score
                    ├─ hash        64-bit dHash (hex)
                    └─ publish photo.scored (PRODUCER span, injects traceparent)
               ─► ack | nak(delay) | term
```

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `NATS_URL` | `nats://nats:4222` | NATS server with JetStream |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4318` (OTel default) | OTLP/HTTP endpoint, in compose `http://otel-collector:4318` |
| `SPOOL_DIR` | `/spool` | Shared volume; the scorer only reads files inside it |

The standard `OTEL_*` variables (`OTEL_RESOURCE_ATTRIBUTES`, `OTEL_SERVICE_NAME`,
…) are honoured by the SDK.

## JetStream

- Stream `PHOTOS` (subjects `photo.>`, `burst.>`, file storage) is created
  if missing; an existing stream is never changed.
- Durable pull consumer `scorer`, filter `photo.ingested`, explicit ack,
  `AckWait` 30 s, `MaxDeliver` 6.
- Outcomes:
  - **ack** after `photo.scored` was published (`Nats-Msg-Id: photo.scored:<sha256>`,
    so a redelivery cannot publish twice).
  - **term** when retrying cannot help: bad payload, path outside `/spool`,
    file missing, file not decodable.
  - **nak with delay** (2, 4, 8, 16, 30 s) for everything else, e.g. a
    permission error on the volume or NATS publish timeout.

## photo.scored

```json
{
  "v": 1,
  "sha256": "…",
  "captured_at": "2026-11-09T10:00:00Z",
  "features": {
    "sharpness": { "variance": 1935.47, "score": 0.9509 },
    "exposure": { "mean": 0.4958, "highlights_clipped": 0.0183, "shadows_clipped": 0.0209, "score": 0.914 },
    "eyes": { "state": "open|closed|no_face", "confidence": 0.9, "faces": 1 },
    "dhash": "554a9695cab9aa2b"
  },
  "timings_ms": { "decode": 48.2, "faces": 52.6, "sharpness": 2.0, "exposure": 0.2, "hash": 2.3 }
}
```

All scores are 0..1, higher is better. Hamming distance between two `dhash`
values ≤ ~6 means "near-identical frame".

## Telemetry

- Traces: spans as above. CONSUMER span attributes: `messaging.system=nats`,
  `messaging.destination.name`, `messaging.operation.type=process`,
  `messaging.consumer.group.name=scorer`, `messaging.message.id` (the
  incoming `Nats-Msg-Id`), `messaging.nats.message.delivery_count`, plus
  `keeper.photo.sha256` and `keeper.outcome` (`scored|retry|terminated`).
- Metric: `keeper.stage.duration` histogram in seconds, `stage=score`.
- Logs: stdlib `logging` → OTel `LoggingHandler` (OTLP, trace-correlated)
  and stdout (with `trace_id`/`span_id` appended).

## Develop and test

`uv` is only needed inside Docker:

```bash
docker build --target test -t keeper-scorer-test scorer/ && docker run --rm keeper-scorer-test
docker build -t keeper-scorer scorer/
```

To change dependencies, edit `pyproject.toml` and relock:

```bash
docker run --rm -v "$PWD/scorer":/w -w /w ghcr.io/astral-sh/uv:0.9.30-python3.13-bookworm-slim uv lock
```

`opencv-python-headless` is pinned below 5: OpenCV 5 dropped
`CascadeClassifier` and the bundled Haar cascades.
