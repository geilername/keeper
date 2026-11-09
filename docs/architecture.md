# Keeper architecture reference

What runs on `main`, for reading alongside the code. The overview, the
diagram and the trace model summary are in the [README](../README.md#architecture).

## Request path of one photo

1. `camera-sim` uploads a JPEG or Sony ARW over FTP to `sftpgo` (user
   `camera`, home `/spool/incoming`).
2. On a successful upload, SFTPGo calls `POST http://ingest:8080/hooks/sftpgo`
   synchronously, before it answers the FTP client. A non-2xx answer makes
   SFTPGo delete the file and fail the transfer; camera-sim retries each frame
   up to 3 times. Contract: [sftpgo/README.md](../sftpgo/README.md).
3. `ingest` hashes the file (sha256), hard-links it to
   `/spool/photos/<sha256>.<ext>` (`.jpg` or `.arw`), inserts a row into
   `ingest.photos`, writes an upright JPEG preview to
   `/spool/previews/<sha256>.jpg` (for an ARW, from its embedded preview) and
   the thumbnail, and publishes `photo.ingested`. A known sha256 is answered as
   `duplicate` (or `resumed`, when an earlier attempt stopped before
   publishing) and is not published again.
4. `scorer` reads the photo from the spool and publishes `photo.scored`.
5. `culler` holds `photo.scored` messages unacked while their burst window is
   open, then publishes `burst.decided` and one `photo.decided` per member,
   and acks.
6. `publisher` creates the Lightroom asset, uploads the original and sets the
   rating (with `LIGHTROOM_MODE=adobe`: and adds it to the collection picked
   in the console; see [Adobe Lightroom](#adobe-lightroom)), then publishes
   `photo.published`. A failed attempt publishes
   `photo.failed` and naks the message for redelivery.
7. `gateway` consumes every subject into its Postgres read model and pushes
   each change to the console over SSE.
8. `indexer` folds every `photo.*` event, with the same code as the gateway
   (`internal/photostate`), into one Elasticsearch document per photo, which
   the gateway's `/api/search` queries.

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
| `indexer` | indexer | `photo.>` | 30 s | unlimited (`-1`) | 2, 5, 15 s, then 30 s |

The `indexer` writes each document with a read-modify-write guarded by
`if_seq_no`/`if_primary_term`, so concurrent and redelivered events never
overwrite each other. Its index `keeper-photos` is derived data: when the
indexer has to create the index, it deletes its durable, and the stream
replays into the new index.

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
photo.ingested  { v, sha256, file_name, path, original_path, media_type, size_bytes, captured_at, camera_model? }
photo.scored    { v, sha256, captured_at, features, timings_ms }
burst.decided   { v, burst_id, members[sha256], best, reason, close_reason }
photo.decided   { v, sha256, burst_id, bucket, rating, reasons[] }
photo.published { v, sha256, lightroom_asset_id, attempt }
photo.failed    { v, sha256, attempt, error_class, error?, retry_at? }
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
| `error` | optional words for the console ("Lightroom refused collection Talks"); the read model shows it instead of `error_class` |
| `error_class` | `lightroom_5xx`, `lightroom_429`, `timeout`, `unreachable`, `conflict`, `lightroom_4xx`, `spool_unreadable`; Adobe only: `lightroom_auth` (not connected or token expired, retried), `lightroom_catalog` (catalogue ID changed, retried), `no_target` (no collection chosen, retried), `album_refused` (permanent) |

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

The rules are in [`internal/policy`](../internal/policy/policy.go). There is
one scoring profile, **conference**, and it judges every frame against the
other frames of its own burst:

- **Unusable** frames are rejects whatever the burst: too blurred to use
  (sharpness < 0.25), nearly black, or blown out.
- Among the usable frames, a frame is a **reject** when it is clearly worse
  than its siblings: blurred next to the sharpest frame (< 60 %), more than one
  stop off the burst's middle exposure (for an even count, the nearer of the
  two middle values, so two exposure groups after a light change are both
  fine), or eyes closed where another frame has them open.
- Every burst with a usable frame keeps exactly one **best** frame, even when
  the whole burst shares a problem: darkness shared by the whole burst is the
  venue, and downcast eyes shared by the whole burst are a moment, not a blink.
  The best frame is ranked by open eyes (when the burst has a blink), then a
  visible face, then sharpness, then exposure nearest the burst's middle.
- The other usable frames are **keepers**.

Each decision carries its reasons in words, and the `burst.decide` span
records `keeper.policy.profile`.

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
| indexer | `process photo.*` → `GET /{index}/_doc/{id}`, `PUT /{index}/_doc/{id}` (Elasticsearch client spans) |

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
| `keeper.lightroom.album.adds` | counter | `result`: `ok`, `no_target`, `refused`, `error` | publisher (Adobe mode) |

## HTTP endpoints

| Service | Endpoint | Purpose |
|---|---|---|
| gateway | `GET /` | console (static files embedded with `go:embed`) |
| gateway | `GET /api/photos?limit=n` | latest photos (default 100, at most 500) |
| gateway | `GET /api/photos/{sha}` | one photo with burst, siblings, `trace_id`, `trace_url` |
| gateway | `GET /api/bursts/{id}` | one burst |
| gateway | `GET /api/search?q=&bucket=&stage=&burst=&limit=n` | photo search in Elasticsearch: `{total, photos}`; `q` in `query_string` syntax (`blur`, `"eyes closed"`, `features.sharpness.score:>0.5`, `attempts:>2`) |
| gateway | `GET /api/events` | SSE, `event: photo` per change |
| gateway | `GET /thumbs/{sha}.jpg` | thumbnail |
| gateway | `GET /auth/lightroom/login`, `GET /auth/lightroom/callback` | Adobe IMS sign-in (authorization code; `state` checked against a cookie) |
| gateway | `GET /api/lightroom/status` | `{state: not_configured\|not_connected\|connected\|expired, mode, account, expires_at, target}` |
| gateway | `GET /api/lightroom/collections` | the user's collections `{id, name, path}`, path through collection sets; 409 when not connected |
| gateway | `PUT /api/lightroom/target` | `{"album_id": …}` (JSON only), must be one of the collections |
| gateway | `GET/POST /api/demo/faults`, `GET /api/demo/stats` | console fault panel, forwarded to lightroom-mock's `/admin/faults` and `/admin/stats`; only with `DEMO_CONTROLS=1` |
| camera-sim | `POST /shoot?set=<name>[&dup=1]`, `GET /sets` | demo driver |
| lightroom-mock | `GET/POST /admin/faults` | fault config: `error_rate` (0..1), `status` (default 503), `latency_ms`; a POST replaces the whole config |
| lightroom-mock | `GET /admin/stats` | assets, masters, ratings, create replays, injected faults |
| lightroom-mock | `POST /admin/reset` | clears faults and empties the catalogue |
| lightroom-mock | `/v2/catalog`, `/v2/catalogs/{catalog_id}/assets/…` | Lightroom-shaped API; faults apply here only |
| ingest | `POST /hooks/sftpgo` | SFTPGo upload hook (compose network only) |

The Lightroom asset ID is the first 32 hex characters of the sha256, so a
replayed create finds the existing asset instead of making a second one.

## Adobe Lightroom

`LIGHTROOM_MODE=mock` (the default, and what compose runs) sends photos to
`lightroom-mock`. `LIGHTROOM_MODE=adobe` sends them to the Lightroom Partner
API at `https://lr.adobe.io` (`internal/lightroom.Adobe`), into one existing
collection the user picks in the console.

**Sign-in.** The gateway runs the Adobe IMS authorization-code flow
(`/auth/lightroom/login` → IMS → `/auth/lightroom/callback`) with the scopes
`openid,AdobeID,lr_partner_apis,offline_access`. The callback checks `state`
against a one-use, HttpOnly cookie, exchanges the code and stores access and
refresh token sealed with AES-256-GCM (key `KEEPER_TOKEN_KEY`) in
`gateway.lightroom_auth`. Once a minute it refreshes the access token when it
expires within 10 minutes. The OAuth routes are not traced, so the code never
lands in a span.

**Gateway → publisher.** The JetStream key-value bucket `keeper-lightroom`
(`internal/lrlink`, history 1, file storage) holds two keys: `target`, the
chosen collection as JSON, and `token`, the current access token sealed with
the same key. The gateway writes both; the publisher watches the bucket, so a
new collection or token is used on the next attempt without a restart. The
refresh token never leaves the gateway.

**Publishing.** Before anything goes into the catalogue the publisher needs a
target; without one the photo waits (`no_target`, retried every 30 s). Then:
create the asset (a replay gets 403 "already exists" and reads the asset
back), upload the original (`image/jpeg`, or `application/octet-stream` for
raws as the spec requires, with its own `LIGHTROOM_UPLOAD_TIMEOUT`, default
5 min, while the handler marks the message in progress every 10 s), then add
it to the collection. Adding an asset that is already in the collection counts
as done: every asset Adobe did not add is looked up in the collection. A 403
for the collection itself ends the photo as `album_refused` with the words
"Lightroom refused collection <name>".

**Ratings.** The Partner API cannot change an asset after it is created
(there is no PATCH and no rating on create), so in Adobe mode the rating stays
in Keeper; the `publish` span records `keeper.lightroom.rating_written=false`.

