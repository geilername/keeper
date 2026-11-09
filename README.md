# Keeper

Keeper keeps the best frame of every burst. Photos leave the camera over FTP,
are scored for sharpness, exposure, open or closed eyes and similarity,
grouped into bursts and rated (best 5★, keeper 3★, reject 1★), then published
to Lightroom exactly once. A live console shows every photo moving through the
pipeline, explains each rating in words, and links each photo to its one
end-to-end OpenTelemetry trace in Dash0.

Built for a hackathon. The Lightroom target is a mock with fault injection
(`lightroom-mock`); no real Lightroom account is used.

- [Prerequisites](#prerequisites)
- [Quick start](#quick-start)
- [Demo script](#demo-script)
- [Architecture](#architecture) (details in [docs/architecture.md](docs/architecture.md))
- [Repository layout](#repository-layout)
- [Development](#development)
- [AI usage](#ai-usage)
- [Data and licences](#data-and-licences)

## Prerequisites

- **Docker with Compose v2** (Docker Desktop, or Docker Engine with the
  compose plugin). Everything, including the Go and Python builds, runs in
  containers.
- **curl** for the demo script; **jq** makes the JSON readable (optional).
- **A Dash0 dataset and an auth token** with ingest permission, to see traces,
  metrics and logs in Dash0. Optional for a local run: see
  [Running without Dash0](#running-without-dash0).

Host ports used, all bound to `127.0.0.1` only: 8090, 8091, 8092, 8222.

## Quick start

```bash
git clone https://github.com/geilername/keeper.git
cd keeper
cp .env.example .env
```

Fill in `.env`:

| Variable | Required | Meaning |
|---|---|---|
| `DASH0_ENDPOINT` | yes | Dash0 OTLP/HTTP endpoint of your region, e.g. `https://ingress.eu-west-1.aws.dash0.com` |
| `DASH0_AUTH_TOKEN` | yes | Dash0 auth token; only the collector sees it |
| `DASH0_DATASET` | no | Dash0 dataset, default `keeper-demo` |
| `DASH0_TRACE_URL` | no | Trace deep link for the console, with `{trace_id}` as placeholder; without it the console shows the trace ID only |
| `CAMERA_FTP_PASSWORD` | no | FTP password of the `camera` user in SFTPGo, default `camera` |
| `CONSOLE_PORT` | no | Host port of the console, default `8090` |
| `COLLECTOR_DEBUG_VERBOSITY` | no | Collector debug exporter: `basic` (default), `normal` or `detailed` |

`LIGHTROOM_MODE` and the `ADOBE_*` lines in `.env.example` are not read by any
service yet; the publisher always talks to `lightroom-mock`.

Start everything:

```bash
docker compose up -d --build
docker compose ps
```

The first build takes a few minutes (Go services, the Python scorer with
OpenCV). Then:

| What | URL |
|---|---|
| Console (gateway) | <http://127.0.0.1:8090> (`CONSOLE_PORT`) |
| camera-sim | <http://127.0.0.1:8092> (`POST /shoot`, `GET /sets`) |
| lightroom-mock API and admin | <http://127.0.0.1:8091> (`/admin/faults`, `/admin/stats`, `/admin/reset`) |
| NATS monitoring | <http://127.0.0.1:8222> (`/jsz`) |
| Elasticsearch | <http://127.0.0.1:9200> (index `keeper-photos`) |

SFTPGo, NATS client port, Postgres and the collector are reachable only inside
the compose network.

### Running without Dash0

The collector refuses to start without `DASH0_ENDPOINT` and `DASH0_AUTH_TOKEN`.
For a local run, give it a dummy endpoint and read the telemetry from the
collector's debug exporter instead:

```bash
# in .env
DASH0_ENDPOINT=http://127.0.0.1:1
DASH0_AUTH_TOKEN=unused
COLLECTOR_DEBUG_VERBOSITY=detailed
```

```bash
docker compose up -d --build
docker compose logs -f otel-collector
```

With `detailed`, every span, metric and log record is printed with its
attributes. The Dash0 exporter keeps logging connection errors and retrying
from its disk queue; that is expected and does not affect the pipeline.

### Stopping and starting over

```bash
docker compose down        # keep data
docker compose down -v     # also delete the spool, NATS, Postgres, Elasticsearch and collector-queue volumes
```

Ingest remembers every photo by its sha256. Shooting the same set again on a
stack that already saw it produces duplicates only, so run `docker compose
down -v` before rehearsing the demo from the start.

## Demo script

The three demo sets (`beach-dog`, `road-dog`, `island-dog`) have six frames
each, 0.3 s apart, with some frames degraded on purpose (blur, under- and
overexposure). A burst is decided after 20 s without a new frame, so photos
reach Lightroom roughly 20 s after the last frame.

Start from a clean stack (`docker compose down -v && docker compose up -d
--build`) and open the console at <http://127.0.0.1:8090>.

### 1. Shoot a burst, with a duplicate

```bash
curl -s http://127.0.0.1:8092/sets | jq
curl -s -X POST 'http://127.0.0.1:8092/shoot?set=beach-dog&dup=1' | jq
```

`dup=1` sends the first frame a second time under a new name
(`beach-dog-01-dup.jpg`). The camera-sim answers when the upload is done, with
the list of uploaded files and the trace ID of its own `shoot` trace.

### 2. Watch the console

The console updates live over SSE. Each photo moves through the stages
ingested → scored → decided → published. The duplicate does not appear as a
seventh photo: ingest recognises the sha256 and records `ingest duplicate` in
the existing photo's trace. After the burst closes, one frame is rated best
(5★), the frames that pass every check are keepers (3★), and blurred or badly
exposed frames are rejects (1★). Click a photo for its record sheet: rating,
reasons in words, scorer readings and its journey.

The same data as JSON:

```bash
curl -s http://127.0.0.1:8090/api/photos | jq -r '.[] | [.file_name, .stage, .bucket, .rating, .trace_id] | @tsv'
```

Search the reasons and readings (Elasticsearch `query_string` syntax; `bucket`,
`stage` and `burst` are exact filters):

```bash
curl -s 'http://127.0.0.1:8090/api/search?q=blur' | jq -r '.photos[] | [.file_name, .bucket, .reasons[0]] | @tsv'
curl -s -G http://127.0.0.1:8090/api/search --data-urlencode 'q=features.eyes.state:closed AND features.sharpness.score:>0.5'
```

### 3. Open a trace

On the record sheet, **Open trace in Dash0** opens the photo's trace in a new
tab (when `DASH0_TRACE_URL` is set). The trace ID is the first 32 hex
characters of the photo's sha256, so it can also be computed from the file:

```bash
shasum -a 256 demo/sets/beach-dog/beach-dog-01.jpg | cut -c1-32
```

One trace holds the whole life of the photo: the `photo <file>` root span from
ingest, the scorer's stage spans, the culler's `decision`, the publisher's
Lightroom calls and the gateway's projection, connected through NATS.

Without Dash0, search the collector output for the trace ID:

```bash
docker compose logs otel-collector | grep -A3 <trace_id>
```

### 4. Break Lightroom, watch the retries, heal

Make every Lightroom API call fail with 503 (the admin endpoints are never
faulted):

```bash
curl -s -X POST http://127.0.0.1:8091/admin/faults \
  -H 'Content-Type: application/json' -d '{"error_rate": 1, "status": 503}' | jq
```

Shoot the next burst:

```bash
curl -s -X POST 'http://127.0.0.1:8092/shoot?set=road-dog' | jq
```

About 20 s later the burst is decided and publishing starts failing. The
publisher retries each photo after 2 s, 5 s, 15 s and then every 30 s, and it
retries indefinitely: an outage delays photos, it never drops them. Only a
permanent error (a 4xx conflict from Lightroom) ends a photo as "not
published". In the console the photos show *Retrying* with the attempt count
and the time of the next try, and the banner says that Lightroom is not
answering and nothing is lost. In the trace, every failed attempt is a red
`publish` span. Dash0 check rules for publish failures are being added as
code (in progress).

Heal it (`{}` clears every fault):

```bash
curl -s -X POST http://127.0.0.1:8091/admin/faults \
  -H 'Content-Type: application/json' -d '{}' | jq
```

Within 30 s every waiting photo is published. Check that Lightroom holds each
photo exactly once:

```bash
curl -s http://127.0.0.1:8091/admin/stats | jq
```

`assets` is the number of distinct photos shot so far (12 after `beach-dog`
with its duplicate and `road-dog`), `with_master` matches it, `ratings` counts
them by star rating, `faults_injected` counts the refused requests, and
`create_replays` counts create calls that found the asset already there.

A latency fault is set the same way, e.g. `{"latency_ms": 3000}`.

### 5. Optional: stop the scorer, watch the backlog drain

```bash
docker compose stop scorer
curl -s -X POST 'http://127.0.0.1:8092/shoot?set=island-dog' | jq
```

The photos stay at *Waiting for scorer*; after 20 s the console says so in its
banner. The messages wait in JetStream:

```bash
curl -s 'http://127.0.0.1:8222/jsz?consumers=true' \
  | jq '.account_details[].stream_detail[].consumer_detail[] | {name, num_pending, num_ack_pending}'
```

Start the scorer again and the backlog drains; the burst is decided and
published as usual:

```bash
docker compose start scorer
```

## Architecture

Services talk through one NATS JetStream stream, `PHOTOS` (subjects `photo.>`
and `burst.>`, file storage). Only the edges are synchronous: the SFTPGo
upload hook, the Lightroom API and the browser. Photo bytes stay on a shared
volume; events carry the sha256 and a path.

```mermaid
flowchart LR
  cam["camera-sim<br/>(or a camera)"] -- FTP --> sftp[SFTPGo]
  sftp -- "upload hook<br/>HTTP" --> ingest
  ingest -- photo.ingested --> scorer["scorer<br/>(Python)"]
  scorer -- photo.scored --> culler
  culler -- "photo.decided" --> publisher
  culler -. burst.decided .-> gateway
  publisher -- "HTTP" --> lr[lightroom-mock]
  publisher -- "photo.published<br/>photo.failed" --> gateway
  ingest -. photo.ingested .-> gateway
  scorer -. photo.scored .-> gateway
  culler -. photo.decided .-> gateway
  gateway -- "REST + SSE" --> browser[console]
  browser -- "Open trace" --> dash0[(Dash0)]
  ingest --- pg[(Postgres)]
  gateway --- pg
  indexer["indexer<br/>(every photo.* event)"] --- es[(Elasticsearch)]
  gateway -- "/api/search" --> es
```

Every arrow labelled with a subject goes through JetStream. Every service
sends traces, metrics and logs over OTLP to `otel-collector`, which batches,
buffers to disk and exports to Dash0 (plus a debug exporter to stdout).

### Services

| Service | Language | Owns | In | Out |
|---|---|---|---|---|
| `sftpgo` | off the shelf (SFTPGo 2.7.6) | FTP endpoint, `/spool/incoming` | FTP uploads by user `camera` | upload hook `POST ingest:8080/hooks/sftpgo` |
| `camera-sim` | Go | demo burst sets (`demo/sets`, read-only) | `POST /shoot?set=…[&dup=1]`, `GET /sets` | FTP uploads to SFTPGo |
| `ingest` | Go | Postgres schema `ingest` (`ingest.photos`, sha256 primary key), `/spool/photos`, `/spool/thumbs` | upload hook | `photo.ingested` |
| `scorer` | Python 3.13 | nothing (stateless) | `photo.ingested` | `photo.scored` |
| `culler` | Go | open burst windows, in memory; their messages stay unacked until decided | `photo.scored` | `burst.decided`, `photo.decided` |
| `publisher` | Go | nothing beyond JetStream redelivery | `photo.decided` | Lightroom API calls, `photo.published`, `photo.failed` |
| `lightroom-mock` | Go | in-memory catalogue, fault config | Lightroom-shaped REST (`/v2/…`), admin API | — |
| `gateway` | Go | Postgres schema `gateway` (read model) | every subject (`photo.>`, `burst.>`) | REST, SSE, the embedded console, `/api/search` |
| `indexer` | Go | Elasticsearch index `keeper-photos` (derived, rebuilt from the stream) | `photo.>` | — |
| `otel-collector` | collector-contrib 0.165.0 | Dash0 token, disk-backed send queue | OTLP from every service | OTLP/HTTP to Dash0, debug exporter |
| `nats` | NATS 2.15.2 | stream `PHOTOS` | — | — |
| `postgres` | Postgres 18.6 | schemas `ingest`, `gateway` | — | — |
| `elasticsearch` | Elasticsearch 9.5.3, single node | index `keeper-photos` | — | — |
| `spool-init` | Alpine | creates the spool directories, owned by uid 1000 | — | runs once |

All Go services are one Go module with one `cmd/<service>` each, built from
one `Dockerfile` (`--build-arg SERVICE=…`) and run as separate containers.
They share `internal/events` (contracts), `internal/natsx` (JetStream with
trace propagation) and `internal/telemetry`.

### Trace model

- **One trace per photo.** `trace_id` is the first 16 bytes of the photo's
  sha256 (its first 32 hex characters). Ingest starts the root span
  `photo <file>` with `span_id` = sha256 bytes 16–24. Any service can derive
  both IDs from the sha256 alone, so restarts and redeliveries land in the
  same trace.
- **Across NATS.** `natsx.Publish` opens a PRODUCER span `publish <subject>`
  and writes the W3C `traceparent` into the NATS message headers;
  `natsx.Consume` opens a CONSUMER span `process <subject>` as its child, with
  `messaging.*` attributes and the delivery count. A message without
  `traceparent` is attached under the photo's derived root.
- **Go ↔ Python.** The scorer extracts and injects the same W3C headers with
  the OpenTelemetry Python SDK. The Go carrier writes the header name in
  lowercase (`traceparent`), which the Python propagator requires, and reads
  either spelling.
- **Bursts.** The culler's `burst.decide` span is the root of its own trace,
  with one span link per member photo; each photo's `decision` span links back
  to it.
- **Logs** carry `trace_id` and `span_id` (Go: `slog` through the `otelslog`
  bridge; Python: the OTel `LoggingHandler`).

Event contracts, consumers, spans and metrics are listed in
[docs/architecture.md](docs/architecture.md).

### Spool layout

One named volume, `spool`, mounted at `/spool`. Every service runs as uid
1000.

```
/spool/incoming/<file>        SFTPGo upload target; ingest removes the file once handled
/spool/photos/<sha256>.jpg    original, content-addressed, written once by ingest
/spool/thumbs/<sha256>.jpg    480 px thumbnail for the console, written by ingest
```

`ingest` is the only writer of `photos/` and `thumbs/`; `scorer`, `publisher`
and `gateway` mount the volume read-only. On `main`, ingest accepts JPEG
uploads only (other files are acknowledged and ignored); support for Sony ARW
raw files is in progress.

## Repository layout

```
keeper/
├─ compose.yaml               the whole system
├─ .env.example               settings to copy into .env
├─ Dockerfile                 one image recipe for every Go service
├─ go.mod, go.sum             one Go module for all Go services
├─ cmd/
│  ├─ camera-sim/             demo camera: replays a burst set over FTP
│  ├─ ingest/                 SFTPGo hook, sha256, thumbnails, photo.ingested
│  ├─ culler/                 burst windows and decisions
│  ├─ publisher/              Lightroom publishing with retries
│  ├─ lightroom-mock/         Lightroom-shaped catalogue with fault injection
│  ├─ indexer/                Elasticsearch photo index from the stream
│  └─ gateway/                read model, REST, SSE, search, serves the console
├─ internal/
│  ├─ events/                 event contracts (JSON, versioned)
│  ├─ natsx/                  JetStream publish/consume with trace context
│  ├─ telemetry/              OTel setup, derived photo trace IDs
│  ├─ policy/                 the culling rules (pure functions)
│  ├─ photostate/             events → photo state (gateway and indexer)
│  ├─ search/                 Elasticsearch client and index mapping
│  ├─ lightroom/              Lightroom client (+ lightroomtest fake)
│  └─ spool/                  spool paths
├─ scorer/                    Python scorer (uv, OpenCV), tests, Dockerfile
├─ web/                       console: static HTML/CSS/JS, embedded in gateway
├─ observability/             collector config
├─ sftpgo/                    SFTPGo config, user, upload hook
├─ demo/                      burst sets, photo sources, generator scripts
├─ docs/architecture.md       contracts, consumers, spans, metrics
├─ PRODUCT.md, DESIGN.md      console product brief and design system
├─ .impeccable/               design-skill records for the console
└─ HACKATHON-PLAN.md          the plan written before the hackathon
```

## Development

Go (version in `go.mod`), from the repository root:

```bash
go vet ./...
go test ./...
```

The NATS integration tests skip unless `NATS_URL` points at a NATS server with
JetStream:

```bash
docker run -d --rm --name keeper-nats-test -p 127.0.0.1:14222:4222 nats:2.15.2-alpine -js
NATS_URL=nats://127.0.0.1:14222 go test -race ./internal/natsx/ ./cmd/culler/
docker stop keeper-nats-test
```

The indexer's integration test also needs Elasticsearch; it recreates the
`keeper-photos` index and the `indexer` durable, so point it at throwaway
containers:

```bash
docker run -d --rm --name keeper-es-test -p 127.0.0.1:19200:9200 -e discovery.type=single-node \
  -e xpack.security.enabled=false -e ES_JAVA_OPTS="-Xms512m -Xmx512m" elasticsearch:9.5.3
ELASTICSEARCH_URL=http://127.0.0.1:19200 NATS_URL=nats://127.0.0.1:14222 go test -race -run Integration ./cmd/indexer/
docker stop keeper-es-test
```

The scorer's tests run in its Docker build (`test` stage); no local Python is
needed:

```bash
docker build --target test -t keeper-scorer-test scorer/ && docker run --rm keeper-scorer-test
```

More on the scorer in [scorer/README.md](scorer/README.md), on SFTPGo and the
upload hook in [sftpgo/README.md](sftpgo/README.md), and on the demo sets in
[demo/sets/README.md](demo/sets/README.md).

Rebuild and restart one service after a change:

```bash
docker compose up -d --build publisher
```

## AI usage

Hackathon rule 5 asks for disclosure.

- **Assistant:** Claude Code with Claude Opus 5.5.
- **Used for:** planning, writing code, writing tests and running reviews.
  Parallel Claude Code sessions built individual services (for example the
  scorer, lightroom-mock, camera-sim, culler, publisher and gateway) and
  documentation, including this README. Commits with AI-written content carry
  `Co-Authored-By: Claude …` trailers.
- **Console design:** the [impeccable](https://github.com/pbakaus/impeccable)
  design skill, which produced `PRODUCT.md`, `DESIGN.md` and the records in
  `.impeccable/`.
- **Review:** the author reviewed changes before committing them.

> **TODO (author): replace this line with your own description of how you
> reviewed the AI-generated changes.**

## Data and licences

- Code: [MIT](LICENSE).
- Demo photos: derived from CC0 photos on Wikimedia Commons; sources,
  authors and checksums in [demo/SOURCES.md](demo/SOURCES.md). No people are
  recognisable in them, and none of the original EXIF (camera, GPS) is kept.
- Fonts: Archivo and Red Hat Mono, SIL Open Font License 1.1, see
  [web/static/fonts/LICENSE.md](web/static/fonts/LICENSE.md).
- No private data: no real Lightroom account, no Dash0 token and no `.env` are
  in the repository (`.env` is gitignored).
