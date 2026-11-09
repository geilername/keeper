# Hackathon plan — camera → cull → Lightroom, observable end to end

Working title: **Keeper** (keeps the best frame of every burst). Rename freely.

Status: pre-hackathon notes. Rule 1 allows ideas, notes, sketches and
research before the start, so this file is fine. Code, compose files,
DESIGN.md and the repo scaffold are not: all of that starts after the
kickoff (§10).

Setup: solo, demo on a laptop with Docker, Dash0 as the observability
backend, the AI assistant disclosed.

---

## 1. Goal and the demo in one paragraph

A burst of photos leaves a "camera" and lands in Lightroom, with only the
best frame of each burst starred. The audience watches every photo move
through the pipeline live in a calm, photo-first console. One click on any
photo opens its trace in Dash0: one waterfall per photo across six
services, from FTP upload to Lightroom. Then Lightroom gets broken on stage
(fault injection). Retries appear in the same trace, a Dash0 check rule
fires, and the pipeline heals once the fault is lifted, with no photo lost
or duplicated.

**What gets judged is this:** the end-to-end flow, a microservice design
that is easy to explain, and OpenTelemetry built in natively (traces,
metrics and logs, correlated). Everything else is cut (§2).

## 2. Scope

**In**

- Camera upload over FTP(S) into SFTPGo (off-the-shelf).
- A camera simulator that uploads a prepared burst set on demand. This is
  the demo driver. A real camera on stage is a stretch goal.
- Content-addressed ingest (sha256): duplicates are detected, never
  uploaded twice.
- Scoring: sharpness, exposure, eyes open/closed, perceptual hash.
- Burst grouping (time gap + dhash) and one decision per burst: best (5★),
  keeper (3★), reject (1★).
- Publishing to Lightroom through an adapter. The demo default is a
  Lightroom mock with fault injection. The real Adobe API is optional.
- Live console (SSE), photo detail ("why this rating"), and a deep link to
  the photo's Dash0 trace.
- Native OTel: traces, metrics and logs from every service, through one
  collector, to Dash0. Dashboards and check rules live as code in the repo.

**Out** (deliberately)

- Multi-tenancy, auth, user accounts.
- Configurable rating modes and scoring profiles: one fixed policy.
- Album routing per shoot, extra albums, reject staging,
  delete-after-upload, evaluation tooling, camera connection monitor.

## 3. Architecture

```
 camera-sim ──FTP──► SFTPGo ──upload hook (HTTP)──► ingest ──┐
 (or real camera)                                           │ photo.ingested
                                                            ▼
                                   ┌──────────── NATS JetStream ────────────┐
                                   │                                        │
                       photo.ingested                             photo.decided
                                   ▼                                        ▼
                                scorer ──photo.scored──► culler ──►    publisher ──HTTP──► lightroom-mock
                               (Python)                 (bursts)          │                (or Adobe API)
                                                                          ▼
                                                                  photo.published / photo.failed
                                   │                                        │
                                   └───────── all subjects ─────────► gateway (read model + SSE + UI)
                                                                          │
                                                                    browser console ──"open trace"──► Dash0

 every service ──OTLP──► otel-collector ──OTLP/HTTP (buffered)──► Dash0
 shared volume /spool: photo bytes + thumbnails (ingest writes, scorer/publisher/gateway read)
 Postgres: one instance, one schema per service that keeps state (ingest, gateway)
```

### 3.1 Services

| Service | Lang | Owns | In | Out |
|---|---|---|---|---|
| `sftpgo` | off-the-shelf | FTP endpoint, `/spool/incoming` | camera uploads | HTTP upload hook → ingest |
| `camera-sim` | Go | demo burst sets | `POST /shoot?set=…` | FTP uploads |
| `ingest` | Go | `ingest.photos` (sha256 PK), thumbnails | upload hook | `photo.ingested` |
| `scorer` | Python | nothing (stateless) | `photo.ingested` | `photo.scored` |
| `culler` | Go | open burst windows (in memory, durable via unacked msgs) | `photo.scored` | `photo.decided`, `burst.decided` |
| `publisher` | Go | nothing beyond JetStream redelivery | `photo.decided` | `photo.published`, `photo.failed` |
| `lightroom-mock` | Go | fake catalogue | Lightroom-shaped REST | admin: `POST /faults` (503 rate, latency) |
| `gateway` | Go | `gateway.photos` read model | all subjects | REST + SSE + embedded UI |
| `otel-collector` | contrib image | — | OTLP from all | Dash0 |
| `nats`, `postgres` | images | — | — | — |

All Go services are **one Go module, one `cmd/` per service**, sharing
`internal/telemetry`, `internal/events` (contracts) and `internal/natsx`
(publish/consume with trace propagation). They are deployed as separate
containers. That gives a microservice runtime with a single place for the
cross-cutting code, which matters when building alone. It should be stated
as such in an ADR (architecture decision record).

### 3.2 Key decisions (each one becomes a short ADR during the hackathon)

- **ADR-1 Event-driven over request chains.** Services talk through NATS
  JetStream subjects. Only the edges are synchronous: the SFTPGo hook, the
  Lightroom API and the browser. Consequences: retries come from
  redelivery, and a slow scorer never blocks ingest.
- **ADR-2 Idempotency by content hash.** The sha256 is the photo ID
  everywhere. Ingest's primary key rejects duplicates. Publishes carry
  `Nats-Msg-Id = <event>:<sha>`, so JetStream drops duplicate publishes
  inside its dedup window. The publisher checks the Lightroom asset ID
  before creating.
- **ADR-3 Bytes on a shared volume, events carry references.** Messages
  stay small: sha, path, metadata. MinIO would be "more microservice", but
  that is time better spent on the demo. Named as a known shortcut.
- **ADR-4 Burst windows without a database.** The culler keeps
  `photo.scored` messages **unacked** while their burst window is open
  (AckWait > max window, extended with `InProgress`). It acks after
  publishing the decision. A culler restart therefore simply gets the open
  windows redelivered. Window rule: close after `idle` (default 20 s) or
  `max_wait` (default 90 s), short for demo pacing.
- **ADR-5 OTel-native, collector in the middle.** Services only know
  `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318`. The collector
  holds the Dash0 token, batches, limits memory and buffers to disk.
- **ADR-6 One trace per photo** (§5). The trace ID is derived from the
  sha256. Checked as pre-kickoff research: spans arriving 17 minutes later,
  from separate processes, attach correctly in Dash0.

### 3.3 Event contracts (`internal/events`, JSON, versioned)

```
photo.ingested  { v, sha256, file_name, path, size_bytes, captured_at, camera_model }
photo.scored    { v, sha256, captured_at, features{ sharpness, exposure, eyes, dhash }, timings_ms }
burst.decided   { v, burst_id, members[sha256], best, reason, close_reason }
photo.decided   { v, sha256, burst_id, bucket, rating, reasons[] }
photo.published { v, sha256, lightroom_asset_id, attempt }
photo.failed    { v, sha256, attempt, error_class, retry_at? }
```

Stream `PHOTOS` with subjects `photo.>` and `burst.>`, file storage, and
one durable consumer per service. W3C `traceparent` travels in the NATS
message headers.

## 4. Pipeline behaviour worth showing

- **Duplicate:** the camera-sim uploads the same file twice. Ingest logs
  it, the trace shows `ingest outcome=duplicate`, and nothing is published
  twice.
- **Burst:** 6 frames in 2 s. One is best (5★), two are keepers (3★), and
  three are rejects (1★: closed eyes, motion blur). The detail sheet says
  why, in words.
- **Fault:** set `lightroom-mock` to 100 % 503. The publisher retries with
  JetStream backoff (`BackOff: 2s, 5s, 15s, 30s`, `MaxDeliver: 8`).
  Console rows show "retrying", Dash0 shows the failed attempts as red
  `publish` spans in the photo trace, and the check rule
  "publish failures > 0 for 1 m" fires. Lift the fault: every photo ends in
  Lightroom exactly once.
- **Scorer down:** stop the scorer. Messages wait in JetStream, the console
  says "waiting for scorer", and nothing is lost. Start it again and the
  backlog drains. (Optional: a fail-open timeout publishes "unscored".)

## 5. Observability design (native OTel → Dash0)

### 5.1 Traces

- **Photo trace:** `trace_id = sha256[0:16]`, root span `photo <file>`
  with `span_id = sha256[16:24]`, created by ingest. Every later step
  starts from the derived parent, so the photo has one waterfall even
  across restarts and redeliveries.
- **Across NATS:** a producer span `publish photo.ingested` (kind
  PRODUCER) carries `traceparent` in the headers. The consumer creates a
  `process photo.ingested` span (kind CONSUMER) as its child.
  Messaging semconv attributes: `messaging.system=nats`,
  `messaging.destination.name`, `messaging.message.id`, delivery count.
- **Bursts:** `culler` emits a `burst.decide` span in its own trace, with
  one span link per member photo. Each photo trace gets a `decision` span
  that links back to it.
- **Scorer stages:** Python spans `extract → decode → faces → sharpness →
  exposure → hash`. If the work runs in a process pool, the pool's worker
  processes have no span context. In that case rebuild the stage spans in
  the parent from measured timings. Otherwise use ordinary spans.
- **Edges:** `otelhttp` on the SFTPGo hook handler, the Lightroom client
  and the gateway API (filter out `/healthz`, `/metrics` and SSE).

### 5.2 Metrics (OTel SDK, no Prometheus client)

| Metric | Type | Why |
|---|---|---|
| `keeper.photo.e2e.duration` | histogram (s) | camera → Lightroom; Dash0 shows a trace's duration as its root span's, so this has to be a metric |
| `keeper.stage.duration{stage}` | histogram | where time goes |
| `keeper.photos{outcome}` | counter | ingested / duplicate / published / failed |
| `keeper.decisions{bucket}` | counter | culling distribution |
| `keeper.publish.attempts{result}` | counter | retries, fault demo |
| `keeper.consumer.pending{consumer}` | observable gauge | backlog per service (JetStream consumer info) |
| `keeper.burst.open` | up-down counter | windows open right now |

Low-cardinality labels only: no sha, path or file name on metrics.

### 5.3 Logs

- **Go:** `slog` through the `otelslog` bridge, so `trace_id`/`span_id`
  come automatically.
- **Python:** the OTel `LoggingHandler`.
- Logs go out as OTLP, not as scraped stdout. They stay readable on stdout
  for `docker compose logs`.

### 5.4 Dash0 as code (`observability/`)

- Collector config: `otlp` receiver, `memory_limiter`, `batch`,
  `resourcedetection`, and `otlphttp` to Dash0 with a `file_storage`-backed
  sending queue. `DASH0_ENDPOINT`, `DASH0_AUTH_TOKEN` and `DASH0_DATASET`
  come from `.env`, never from the repo.
- One dashboard, "Keeper pipeline": photos per minute, e2e p50/p95, stage
  durations, consumer backlog, publish errors, decisions by bucket.
- Check rules: publish failures, consumer backlog > N for 2 m, e2e p95
  > 60 s.
- **Verify right after kickoff:** the import path Dash0 offers for
  dashboards and rules (API, CLI, Terraform or operator CRDs), and the URL
  pattern of a trace deep link for the console button.

## 6. UI (impeccable)

The console is the face of the demo. It serves two audiences: the
photographer ("is it flowing?") and the judges ("show me the
architecture").

- **Stack:** static HTML/CSS/vanilla JS modules, `go:embed` in the
  gateway, SSE for live updates. No build step, no framework.
- **Screens:**
  1. *Flow:* photos as thumbnails moving through the columns
     ingest → scoring → burst → publishing → in Lightroom. Bursts are
     stacked, with the best frame on top.
  2. *Photo sheet:* rating plus the reasons in words, the burst siblings,
     scorer measurements, attempts, and **"Open trace in Dash0"**.
  3. *Fault panel* (demo only, clearly marked): Lightroom 503 on/off,
     latency, "stop scorer" hint.
- **Process:** `npx skills add pbakaus/impeccable`, then `/teach-impeccable`
  at the start, with PRODUCT.md and DESIGN.md written during the
  hackathon. Then build, then `/critique` → `/audit` → `/polish`, and
  `/animate` only for state transitions. Acceptance: no high-priority audit
  findings, readable at 360 px, dark and light.
- **Direction notes (ideas, allowed now):** photo-first and calm. Chrome
  recedes, and state is shown with small "wall label" captions under the
  frames rather than colourful badges. "Calm by default, unmistakable when
  action is needed."

## 7. Repo layout (to create after kickoff)

```
keeper/
├─ README.md               setup, architecture, demo script, AI disclosure
├─ compose.yaml            whole system; `docker compose up` + .env
├─ .env.example            DASH0_*, LIGHTROOM_MODE=mock|adobe, ADOBE_* (optional)
├─ docs/adr/               ADR-1..6
├─ docs/architecture.md    diagram + event contracts
├─ go.mod
├─ cmd/{ingest,culler,publisher,gateway,camera-sim,lightroom-mock}/
├─ internal/{telemetry,events,natsx,store,score-policy,lightroom}/
├─ web/                    console (impeccable)
├─ scorer/                 Python (uv), pyproject, tests
├─ observability/          collector config, dashboard, check rules
├─ sftpgo/                 users + event-hook config
└─ demo/                   burst sets (own/consented photos) + fetch script
```

## 8. Milestones (in order, with cut lines)

| # | Deliverable | Done when |
|---|---|---|
| M0 | Repo, compose with nats/postgres/collector, `internal/telemetry`, a hello span in Dash0 | Span from a container visible in Dash0 |
| M1 | **Walking skeleton:** camera-sim → SFTPGo hook → ingest → NATS → scorer (stub) → culler (pass-through) → publisher → lightroom-mock → gateway list | One photo crosses all services and shows as one trace in Dash0 |
| — | **Cut line A:** demoable "photo flows, traced end to end" | |
| M2 | Real scorer features, burst windows + decision policy, idempotency, JetStream retries, fault injection | Burst demo + fault demo work from the CLI |
| M3 | Console with impeccable: flow, photo sheet, Dash0 deep link, fault panel | Audit clean, works on a phone |
| — | **Cut line B:** full demo story | |
| M4 | Metrics + logs complete, dashboard + check rules as code, e2e metric | Alert fires during the fault demo |
| M5 | README, ADRs, architecture doc, demo video, two rehearsals | A fresh clone + `.env` → demo runs |

If time runs short, ship at the last cut line reached and polish that
instead of starting the next milestone.

**Stretch:** real Sony camera over FTPS on stage, real Adobe Lightroom
adapter, ARW embedded-preview extraction (start with JPEG bursts),
tail-sampling demo in the collector.

## 9. Design notes from research

- sha256 content addressing makes "exactly once" provable. Keep it the
  spine.
- Derived trace IDs work in Dash0. Trace duration = root-span duration, so
  end-to-end latency must be a metric.
- Bursts need both an idle and a max-wait close. Without max-wait, a long
  shoot never decides.
- Scorers that run in process pools lose span context. Rebuild the stage
  spans from timings instead of instrumenting the workers.
- Spool permissions: SFTPGo and the services must agree on uid/umask, or
  ingest cannot read uploads. Pin both in compose.
- A real Sony body verifies FTPS certificates against a hostname. For the
  demo, camera-sim uses plain FTP on the compose network. FTPS with a real
  certificate is stretch work only.
- Fail open: an unavailable scorer must not stop uploads forever. Decide
  explicitly what happens (wait vs. publish unscored).

## 10. Hackathon rules: what this means for the project

| Rule | Consequence |
|---|---|
| 1 Work starts at kickoff | Before kickoff: only this plan, research, account setup (Dash0 dataset + token, optional Adobe credentials). No repo scaffold, no compose file, no DESIGN.md, no code. All code is written after kickoff; nothing is copied from earlier repos or spikes. |
| 3 Public repo + README | README with prerequisites, `.env.example`, `docker compose up`, a demo script, and the architecture diagram. |
| 4 Own data only, nothing private | Demo photos: shot during the hackathon, with consent from everyone pictured, or CC0 images fetched by a script. No real Lightroom account, Dash0 token or `.env` in the repo, the video or screenshots. The demo uses `lightroom-mock` and a dedicated Dash0 dataset. |
| 5 Disclose AI | README section "AI usage": which assistant, for what, and how the output was reviewed. Keep the `Co-Authored-By` commit trailers. |
| 6/7 Understand the code | Write the ADRs yourself and review every AI change before committing. Be able to explain every service and the trace model. Keep the architecture small enough to whiteboard in 2 minutes. |
| 10 IP | Solo, so ownership is clear. Pick a licence at kickoff (e.g. MIT). |

## 11. Before kickoff (allowed prep)

1. Create a Dash0 dataset "keeper-demo" and a token for it. Note the
   region endpoint.
2. Decide the project name.
3. Research only: the SFTPGo EventManager upload hook (HTTP action),
   NATS JetStream `BackOff`/`AckWait`/`InProgress` semantics, the
   `otelslog` bridge, the OTel Python logging handler, and the Dash0
   dashboard/rule import path and trace deep-link URL.
4. Plan the demo photo shoot: 3 bursts (portrait with one blink, a moving
   subject with blur, a static scene), consent from the people in them.
