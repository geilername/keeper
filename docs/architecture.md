# Keeper architecture reference

What runs on `main`, for reading alongside the code. The overview, the
diagram and the trace model summary are in the [README](../README.md#architecture).

## Request path of one photo

1. `camera-sim` uploads a JPEG over FTP to `sftpgo` (user `camera`, home
   `/spool/incoming`).
2. On a successful upload, SFTPGo calls `POST http://ingest:8080/hooks/sftpgo`
   synchronously, before it answers the FTP client. A non-2xx answer makes
   SFTPGo delete the file and fail the transfer; camera-sim retries each frame
   up to 3 times. Contract: [sftpgo/README.md](../sftpgo/README.md).
3. `ingest` hashes the file (sha256), hard-links it to
   `/spool/photos/<sha256>.jpg`, inserts a row into `ingest.photos`, writes
   the thumbnail and publishes `photo.ingested`. A known sha256 is answered as
   `duplicate` (or `resumed`, when an earlier attempt stopped before
   publishing) and is not published again.
4. `scorer` reads the photo from the spool and publishes `photo.scored`.
5. `culler` holds `photo.scored` messages unacked while their burst window is
   open, then publishes `burst.decided` and one `photo.decided` per member,
   and acks.
6. `publisher` creates the Lightroom asset, uploads the original and sets the
   rating, then publishes `photo.published`. A failed attempt publishes
   `photo.failed` and naks the message for redelivery.
7. `gateway` consumes every subject into its Postgres read model and pushes
   each change to the console over SSE.

## Stream and consumers

Stream `PHOTOS`, subjects `photo.>` and `burst.>`, file storage. It is created
by the first service that connects (Go `natsx.EnsureStream`, or the scorer);
an existing stream is never changed.

| Durable | Service | Filter | AckWait | MaxDeliver | Nak delays |
|---|---|---|---|---|---|
| `scorer` | scorer | `photo.ingested` | 30 s | 6 | 2, 4, 8, 16, 30 s |
| `culler` | culler | `photo.scored` | 30 s, `InProgress` every 10 s while held | 8 | 2, 5, 15, 30 s |
| `publisher` | publisher | `photo.decided` | 30 s | unlimited (`-1`) | 2, 5, 15 s, then 30 s |
| `gateway` | gateway | none (all subjects) | 30 s | unlimited (`-1`) | 2, 5, 15 s, then 30 s |

Handler results map to JetStream actions in `internal/natsx`: success → ack;
a permanent error (bad payload, a Lightroom 4xx conflict) → term; anything
else → nak with the delay for that delivery. The culler returns `ErrHold` to
leave a message unacked while its burst is open.

Every publish sets `Nats-Msg-Id = <subject>:<id>` (`events.MsgID`), so
JetStream drops a repeated publish of the same event inside its dedup window.
`photo.failed` includes the attempt in its ID, so each failed attempt is its
own event.

## Event contracts

Defined in [`internal/events/events.go`](../internal/events/events.go); the
scorer mirrors `photo.scored` in Python. JSON, every event carries
`"v": 1`. Messages carry references (sha256, spool path), never photo bytes.

```
photo.ingested  { v, sha256, file_name, path, size_bytes, captured_at, camera_model? }
photo.scored    { v, sha256, captured_at, features, timings_ms }
burst.decided   { v, burst_id, members[sha256], best, reason, close_reason }
photo.decided   { v, sha256, burst_id, bucket, rating, reasons[] }
photo.published { v, sha256, lightroom_asset_id, attempt }
photo.failed    { v, sha256, attempt, error_class, retry_at? }
```

`features` in `photo.scored`:

```json
{
  "sharpness": { "variance": 1935.47, "score": 0.9509 },
  "exposure": { "mean": 0.4958, "highlights_clipped": 0.0183, "shadows_clipped": 0.0209, "score": 0.914 },
  "eyes": { "state": "open|closed|no_face", "confidence": 0.9, "faces": 1 },
  "dhash": "554a9695cab9aa2b"
}
```

Scores are 0..1, higher is better. `timings_ms` holds the scorer's stage
times (`decode`, `faces`, `sharpness`, `exposure`, `hash`).

| Field | Values |
|---|---|
| `bucket` / `rating` | `best` / 5, `keeper` / 3, `reject` / 1 |
| `close_reason` | `idle`, `max_wait` |
| `retry_at` | set while the publisher will retry; absent when the failure is final |

## Bursts and decisions

The culler groups photos by capture time (EXIF `DateTimeOriginal` plus
sub-seconds) and dHash. Settings, all optional environment variables of the
`culler` service:

| Variable | Default | Meaning |
|---|---|---|
| `BURST_TIGHT_GAP` | 1 s | frames this close to a neighbour join its burst regardless of dHash |
| `BURST_GAP` | 2 s | up to this gap, a frame joins the burst if its dHash distance is at most `DHASH_MAX` |
| `DHASH_MAX` | 12 | dHash Hamming distance limit |
| `BURST_IDLE` | 20 s | a burst is decided after this long without a new frame |
| `BURST_MAX_WAIT` | 90 s | a burst is decided at the latest this long after it opened |

The rules are in [`internal/policy`](../internal/policy/policy.go): the best
frame is the sharpest frame that passes every check (eyes not closed, sharp in
absolute terms and relative to the burst's sharpest frame, well exposed);
other passing frames are keepers; failing frames are rejects, each with its
reasons in words. If no frame passes, every frame is a reject.

## Traces

Trace and span IDs of a photo are derived in
[`internal/telemetry/photo.go`](../internal/telemetry/photo.go):

```
trace_id = sha256[0:16]    (bytes; the first 32 hex characters)
root     = sha256[16:24]   (span ID of "photo <file>")
```

Spans in one photo's trace, by service:

| Service | Spans |
|---|---|
| ingest | `photo <file>` (root, linked to the upload-hook server span), `hash`, `thumbnail`, `publish photo.ingested`; for a repeated upload `ingest duplicate` or `ingest resumed` |
| scorer | `process photo.ingested` → `decode`, `faces`, `sharpness`, `exposure`, `hash`, `publish photo.scored` |
| culler | `process photo.scored`, `decision` (linked to the burst), `publish photo.decided` |
| publisher | `process photo.decided` → `publish` (one per attempt, with the Lightroom HTTP client spans) → `publish photo.published` or `publish photo.failed` |
| lightroom-mock | server spans named by route, e.g. `PUT /v2/catalogs/{catalog_id}/assets/{asset_id}` |
| gateway | `process photo.*` for each event it projects |

Outside the photo traces: the culler's `burst.decide` span starts its own
trace and links to every member photo; camera-sim's `shoot <set>` trace (FTP
carries no trace context) returns its trace ID in the `/shoot` response.

Messaging attributes on PRODUCER and CONSUMER spans: `messaging.system=nats`,
`messaging.operation.type`, `messaging.destination.name`,
`messaging.message.id`, `messaging.consumer.group.name` (consumers) and
`messaging.nats.message.delivery_count`.

## Metrics

All through the OpenTelemetry SDK, exported over OTLP. Labels are low
cardinality: no sha256, path or file name.

| Metric | Type | Labels | Emitted by |
|---|---|---|---|
| `keeper.photos` | counter | `outcome`: `ingested`, `duplicate`, `resumed`, `published`, `failed` | ingest, publisher |
| `keeper.stage.duration` | histogram (s) | `stage`: `ingest`, `score`, `cull`, `publish` | ingest, scorer, culler, publisher |
| `keeper.publish.attempts` | counter | `result`: `ok`, `retry`, `failed` | publisher |
| `keeper.decisions` | counter | `bucket` | culler |
| `keeper.burst.open` | up-down counter | — | culler |
| `keeper.photo.e2e.duration` | histogram (s) | — | gateway (capture time to `photo.published`) |
| `keeper.consumer.pending` | observable gauge | `consumer` | gateway (JetStream consumer info) |
| `keeper.lightroom.requests` | counter | `route`, `result` | lightroom-mock |

## HTTP endpoints

| Service | Endpoint | Purpose |
|---|---|---|
| gateway | `GET /` | console (static files embedded with `go:embed`) |
| gateway | `GET /api/photos?limit=n` | latest photos (default 100, at most 500) |
| gateway | `GET /api/photos/{sha}` | one photo with burst, siblings, `trace_id`, `trace_url` |
| gateway | `GET /api/bursts/{id}` | one burst |
| gateway | `GET /api/events` | SSE, `event: photo` per change |
| gateway | `GET /thumbs/{sha}.jpg` | thumbnail |
| camera-sim | `POST /shoot?set=<name>[&dup=1]`, `GET /sets` | demo driver |
| lightroom-mock | `GET/POST /admin/faults` | fault config: `error_rate` (0..1), `status` (default 503), `latency_ms`; a POST replaces the whole config |
| lightroom-mock | `GET /admin/stats` | assets, masters, ratings, create replays, injected faults |
| lightroom-mock | `POST /admin/reset` | clears faults and empties the catalogue |
| lightroom-mock | `/v2/catalog`, `/v2/catalogs/{catalog_id}/assets/…` | Lightroom-shaped API; faults apply here only |
| ingest | `POST /hooks/sftpgo` | SFTPGo upload hook (compose network only) |

The Lightroom asset ID is the first 32 hex characters of the sha256, so a
replayed create finds the existing asset instead of making a second one.
