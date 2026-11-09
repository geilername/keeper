#!/usr/bin/env bash
# Validate Keeper's Dash0 assets (dashboard + check rules) and apply them.
#
#   observability/apply-dash0.sh --dry-run   validate offline: no token, no network
#   observability/apply-dash0.sh             validate, then apply to Dash0
#
# Everything runs in pinned containers (Docker), so nothing needs installing.
# Applying needs, from the environment or .env (never committed):
#   DASH0_API_URL    API endpoint of your region, e.g. https://api.eu-west-1.aws.dash0.com
#   DASH0_API_TOKEN  Dash0 auth token allowed to write dashboards and check rules
#   DASH0_DATASET    target dataset, e.g. keeper-demo
# DASH0_AUTH_TOKEN (the collector's ingest token) is deliberately not used.
set -euo pipefail

DASH0_CLI_IMAGE=${DASH0_CLI_IMAGE:-ghcr.io/dash0hq/cli:v1.19.0}
PROMETHEUS_IMAGE=${PROMETHEUS_IMAGE:-prom/prometheus:v3.7.3}
YQ_IMAGE=${YQ_IMAGE:-mikefarah/yq:4.48.1}
# The CLI image's CA bundle predates Let's Encrypt's newer chains (Dash0's API
# is served under the YR2 intermediate), so the CLI gets a current bundle from
# this image instead.
CA_IMAGE=${CA_IMAGE:-alpine:3.22}

here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
assets="$here/dash0"
tests="$here/dash0-tests"

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; }

dry_run=false
case "${1:-}" in
  --dry-run) dry_run=true ;;
  "") ;;
  -h | --help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

# Read only the three settings above from .env; the environment wins. The file
# is parsed, not sourced, so nothing in it is executed.
if [[ -f "$root/.env" ]]; then
  while IFS='=' read -r key value; do
    case "$key" in
      DASH0_API_URL | DASH0_API_TOKEN | DASH0_DATASET)
        value=${value%\"}; value=${value#\"}; value=${value%\'}; value=${value#\'}
        if [[ -z "${!key:-}" && -n "$value" ]]; then export "$key=$value"; fi ;;
    esac
  done < "$root/.env"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

offline() { docker run --rm --network none "$@"; }
yq() { offline -i -v "$assets:/assets:ro" "$YQ_IMAGE" "$@"; }
# The image runs as nobody, but $work is mktemp's 0700 dir; on native Linux
# Docker the mount keeps that, so run promtool as the dir's owner.
promtool() {
  offline --user "$(id -u):$(id -g)" -v "$work:/work:ro" --entrypoint promtool "$PROMETHEUS_IMAGE" "$@"
}

echo "1/3  Check rules: PromQL syntax and rule unit tests (promtool)"
# promtool reads plain Prometheus rule files: the CRD's .spec is one.
yq '.spec' /assets/check-rules/keeper.yaml > "$work/keeper-rules.yaml"
cp "$tests/check-rules-test.yaml" "$work/"
promtool check rules --lint-fatal /work/keeper-rules.yaml
promtool test rules /work/check-rules-test.yaml

echo "2/3  Dashboard: PromQL syntax of every panel query (promtool)"
# Each query becomes a recording rule so promtool parses it. $__rate_interval
# is a dashboard variable that Dash0 fills in; any duration parses the same.
# shellcheck disable=SC2016 # $__rate_interval is meant literally.
yq '[.spec.panels[].spec.queries[].spec.plugin.spec.query] | to_entries
    | {"groups": [{"name": "dashboard", "rules": map({
        "record": "keeper_dashboard_query_" + (.key | tostring),
        "expr": .value | sub("\$__rate_interval"; "1m")})}]}' \
  /assets/dashboards/keeper-pipeline.yaml > "$work/dashboard-queries.yaml"
promtool check rules --lint-fatal /work/dashboard-queries.yaml

echo "3/3  Dash0 CLI: document validation (dash0 apply --dry-run, offline)"
offline -v "$assets:/assets:ro" "$DASH0_CLI_IMAGE" apply -f /assets --dry-run --agent-mode=false

app_url() {
  local app="https://app.dash0.com"
  if [[ -n "${DASH0_API_URL:-}" ]]; then
    # Same derivation as the Dash0 CLI: app. + the API host's last two labels.
    local host=${DASH0_API_URL#*://}; host=${host%%[:/]*}
    app="https://app.$(awk -F. '{print $(NF-1)"."$NF}' <<<"$host")"
  fi
  printf '%s' "$app"
}

trace_url_hint() {
  local app; app=$(app_url)
  echo "Trace deep link for .env (see observability/README.md):"
  echo "  DASH0_TRACE_URL=$app/goto/traces/explorer?dataset=${DASH0_DATASET:-<dataset>}&trace_id={trace_id}"
}

if $dry_run; then
  echo "Valid. Nothing was sent to Dash0 (--dry-run)."
  trace_url_hint
  exit 0
fi

missing=()
for var in DASH0_API_URL DASH0_API_TOKEN DASH0_DATASET; do
  [[ -n "${!var:-}" ]] || missing+=("$var")
done
if ((${#missing[@]})); then
  echo "Not applied: set ${missing[*]} (environment or .env). See observability/README.md." >&2
  exit 1
fi

echo "Applying to dataset \"$DASH0_DATASET\" at $DASH0_API_URL"
# The token travels as an environment variable, never as an argument, so it
# does not show up in the process list or shell history.
docker run --rm "$CA_IMAGE" cat /etc/ssl/certs/ca-certificates.crt > "$work/ca.pem"
DASH0_AUTH_TOKEN="$DASH0_API_TOKEN" docker run --rm \
  -e DASH0_AUTH_TOKEN -e DASH0_API_URL -e DASH0_DATASET \
  -v "$work/ca.pem:/etc/keeper/ca.pem:ro" -e SSL_CERT_FILE=/etc/keeper/ca.pem \
  -v "$assets:/assets:ro" "$DASH0_CLI_IMAGE" apply -f /assets --agent-mode=false

# The dashboard's ID is fixed in its file, so its link is too.
dashboard_id=$(awk '/dash0.com\/id:/ {print $2; exit}' "$assets/dashboards/keeper-pipeline.yaml")
echo "Dashboard: $(app_url)/goto/dashboards?dashboard_id=$dashboard_id&dataset=$DASH0_DATASET"
trace_url_hint
