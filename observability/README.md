# Observability

Every Keeper service sends traces, metrics and logs over OTLP to one
collector, which forwards them to Dash0. The Dash0 dashboard and check rules
live here as code.

| Path | What it is |
|---|---|
| `collector.yaml` | OpenTelemetry Collector config: OTLP in, Dash0 out (`DASH0_ENDPOINT`, `DASH0_AUTH_TOKEN`, `DASH0_DATASET` from `.env`) |
| `dash0/dashboards/keeper-pipeline.yaml` | Dashboard "Keeper pipeline", as a Perses dashboard CRD |
| `dash0/check-rules/keeper.yaml` | Three check rules, as a PrometheusRule CRD |
| `dash0-tests/check-rules-test.yaml` | promtool unit tests for the check rules |
| `apply-dash0.sh` | Validates everything offline, then applies `dash0/` to Dash0 |

## Dashboard: Keeper pipeline

Two sections.

- **Flow:** photos per minute by outcome, camera-to-Lightroom p50 and p95,
  decisions per minute by bucket, burst windows open.
- **Health:** Lightroom publish attempts per minute by result (ok, retry,
  failed), messages waiting per consumer, p95 time per stage.

## Check rules

| Check rule in Dash0 | Fires when | Why |
|---|---|---|
| Keeper - Lightroom publish retrying | any `result="retry"` publish attempt in the last 2 m, for 1 m | the fault demo: Lightroom returns 503 |
| Keeper - Consumer backlog | more than 20 messages wait for one consumer, for 2 m | scorer stopped, or a stage cannot keep up |
| Keeper - Photos slow to reach Lightroom | camera-to-Lightroom p95 above 60 s, for 5 m | a burst alone is about 20 s, so 60 s means trouble downstream |

Summaries and descriptions follow the console's voice (`PRODUCT.md`): what is
happening, that nothing is lost, and what happens next.

## Apply

Requirements: Docker. The script runs the Dash0 CLI, promtool and yq from
pinned images, so nothing else needs installing.

Validate only (no token, no network inside the containers; the images are
pulled once):

```bash
observability/apply-dash0.sh --dry-run
```

This runs, in order: `promtool check rules` and `promtool test rules` on the
check rules, a PromQL syntax check of every dashboard query, and
`dash0 apply --dry-run` (the Dash0 CLI's own document validation).

Apply to Dash0:

```bash
observability/apply-dash0.sh
```

It reads three settings from the environment, or from `.env` if they are not
set there (`.env` is parsed, not executed, and never committed):

| Variable | Value |
|---|---|
| `DASH0_API_URL` | API endpoint of your region, from Settings → Endpoints → API (e.g. `https://api.eu-west-1.aws.dash0.com`). This is not the OTLP `DASH0_ENDPOINT` (`ingress.…`). |
| `DASH0_API_TOKEN` | A Dash0 auth token allowed to create dashboards and check rules (Settings → Auth Tokens). Not the collector's ingest token: `DASH0_AUTH_TOKEN` is never used for this. |
| `DASH0_DATASET` | Target dataset, the same one the collector writes to (`keeper-demo`). It must exist in your Dash0 organization (Settings → Datasets); otherwise Dash0 links fail with "Invalid parameters: dataset". |

After applying, the script prints the dashboard's link
(`https://app.<domain>/goto/dashboards?dashboard_id=<id>&dataset=<dataset>`)
and the `DASH0_TRACE_URL` line for `.env`.

`dash0 apply` is an upsert. The dashboard carries a fixed
`dash0.com/id`, and each check rule gets a stable ID derived from the
PrometheusRule's `dash0.com/id` (`keeper-checks`) and its name. Running the
script again updates the same assets; it never creates duplicates. Renaming an
alert creates a new check rule; delete the old one by hand
(`dash0 check-rules delete <id>`).

Without Docker, with the Dash0 CLI installed (`brew install --cask dash0hq/dash0-cli/dash0`):

```bash
DASH0_AUTH_TOKEN="$DASH0_API_TOKEN" dash0 apply -f observability/dash0/ --api-url "$DASH0_API_URL" --dataset "$DASH0_DATASET"
```

### Another dataset

Assets belong to a dataset, and the files here name none. Set
`DASH0_DATASET` (and point the collector's `DASH0_DATASET` at the same one so
there is data to show), then run the script again:

```bash
DASH0_DATASET=keeper-rehearsal observability/apply-dash0.sh
```

The same IDs are reused in each dataset, so every dataset gets its own copy.

## Metric names

The services emit OpenTelemetry metrics with dotted names
(`keeper.publish.attempts`). PromQL does not allow dots in a bare metric name,
so Dash0 offers two ways to select them:

1. **The OTel name, through the `otel_metric_name` label** (used here):
   `{otel_metric_name="keeper.publish.attempts", otel_metric_type="sum"}`.
   Dash0 adds `otel_metric_name` and `otel_metric_type` to every metric and
   recommends this form for most queries.
2. **The Prometheus-translated name:** dots become underscores, and a unit
   suffix (`_seconds`) and a counter suffix (`_total`) may be added, e.g.
   `keeper_publish_attempts_total`, `keeper_photo_e2e_duration_seconds`.

We use the first form because the names in the files are then exactly the
names in the Go and Python code, with no guessing about suffixes. Curly-brace
units such as `{photo}` are not part of either name, so it does not matter
that ingest declares `keeper.photos` without a unit and the publisher with
`{photo}`.

On this selector Dash0 serves OTel histograms as native histograms:
`histogram_quantile(0.95, sum(rate({otel_metric_name="…"}[5m])))`, with no
`_bucket` series and no `le` label. `sum by (stage)` never mixes bucket
layouts, because each stage is recorded by exactly one service.

| Metric | OTel type | Attribute used | Recorded by |
|---|---|---|---|
| `keeper.photos` | counter | `outcome` | ingest, publisher |
| `keeper.photo.e2e.duration` | histogram (s) | none | gateway |
| `keeper.stage.duration` | histogram (s) | `stage` | ingest, scorer, culler, publisher |
| `keeper.decisions` | counter | `bucket` | culler |
| `keeper.publish.attempts` | counter | `result` | publisher |
| `keeper.consumer.pending` | observable gauge | `consumer` | gateway |
| `keeper.burst.open` | up-down counter | none | culler |

## Trace deep link (`DASH0_TRACE_URL`)

The console's "Open trace in Dash0" button opens the photo's trace in Dash0's
trace explorer, with the waterfall open on the photo's root span
(`photo <file>`). Set it in `.env`:

```
DASH0_TRACE_URL=https://app.dash0.com/traces/explorer?org=<org-id>&s={dash0_state}
```

`<org-id>` is the `org=` value in the address bar of any Dash0 page. The
gateway fills `{dash0_state}` per photo; `apply-dash0.sh` prints the line.

How `{dash0_state}` works: Dash0 keeps the explorer's view in the `s` query
parameter as JSON, zlib-compressed and base64url-encoded. The gateway builds
that JSON in the shape of a link copied from Dash0
([`cmd/gateway/tracelink.go`](../cmd/gateway/tracelink.go)): the dataset
(`DASH0_DATASET`), a filter `otel.trace.id is <trace id>`, the trace ID and the
focused span (the root span) in base64, the waterfall open on the overview tab.
A test decodes it the way Dash0 does and compares it with a copied link.

Other placeholders, for templates of your own: `{trace_id}` (32 hex) and
`{span_id}` (the root span, 16 hex). The CLI's link format
`https://app.<domain>/goto/traces/explorer?dataset=<dataset>&trace_id={trace_id}`
(from Dash0's [`deeplinks.go`](https://github.com/dash0hq/dash0-api-client-go/blob/main/deeplinks.go))
was rejected by the live organization with "Invalid parameters: dataset", so
the explorer view link is the default.

## Sources

- Dash0 CLI: [README](https://github.com/dash0hq/dash0-cli) and
  [command reference](https://github.com/dash0hq/dash0-cli/blob/main/docs/commands.md)
  (`apply`, asset identifiers, PrometheusRule mapping, asset YAML formats,
  explorer deep links)
- Dash0 docs: [Manage check rules as code](https://www.dash0.com/docs/dash0/monitoring/alerting/manage-check-rules-as-code),
  [Understand OTel metric names](https://www.dash0.com/docs/dash0/telemetry/metrics/understand-otel-metric-names),
  [Common metric query issues](https://www.dash0.com/docs/dash0/telemetry/metrics/common-metric-query-issues)
- Dash0 Go API client: [`deeplinks.go`](https://github.com/dash0hq/dash0-api-client-go/blob/main/deeplinks.go)
