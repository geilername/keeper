#!/usr/bin/env bash
# Build Keeper, load it into a local kind cluster and install the Helm chart.
#
#   deploy/kind-up.sh                                  # build, load, install
#   deploy/kind-up.sh --set lightroomMock.enabled=true  # extra helm flags pass through
#
# Environment:
#   KIND_CLUSTER  kind cluster name            (default: kind)
#   NAMESPACE     namespace for the release    (default: keeper)
#   LAN_IP        address the camera connects to (default: this Mac's en0 address)
#   RENEW_TLS=1   replace the self-signed FTPES/https certificate
#
# Secrets come from .env (DASH0_*, CAMERA_FTP_PASSWORD) and are written to the
# cluster only; nothing is printed or stored in the repo. The Postgres password
# is generated once and kept in the cluster secret.
set -euo pipefail

cd "$(dirname "$0")/.."
CLUSTER=${KIND_CLUSTER:-kind}
NS=${NAMESPACE:-keeper}
CTX="kind-$CLUSTER"
LAN_IP=${LAN_IP:-$(ipconfig getifaddr en0 2>/dev/null || true)}
TAG="dev-$(git rev-parse --short HEAD)$(git diff --quiet HEAD -- cmd internal scorer web go.mod go.sum Dockerfile || echo -dirty)"
APPS=(ingest culler publisher gateway indexer lightroom-mock)

log() { printf '\033[1m▸ %s\033[0m\n' "$*"; }
die() { printf 'kind-up: %s\n' "$*" >&2; exit 1; }

[[ -n "$LAN_IP" ]] || die "could not find this Mac's LAN address; set LAN_IP"
[[ -f .env ]] || die ".env missing (copy .env.example and fill it)"
for tool in kind kubectl helm docker openssl; do command -v "$tool" >/dev/null || die "$tool not found"; done

# .env value without echoing it; strips optional surrounding quotes.
envval() { local v; v=$(grep -E "^$1=" .env | tail -1 | cut -d= -f2-); v=${v%\"}; v=${v#\"}; printf '%s' "$v"; }

# ── cluster ────────────────────────────────────────────────
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "creating kind cluster $CLUSTER with LAN port mappings"
  kind create cluster --name "$CLUSTER" --config deploy/kind/cluster.yaml
fi
docker port "$CLUSTER-control-plane" 2>/dev/null | grep -q '2121' \
  || die "cluster $CLUSTER has no port mappings; recreate it: kind delete cluster --name $CLUSTER && kind create cluster --name $CLUSTER --config deploy/kind/cluster.yaml"

# ── images ─────────────────────────────────────────────────
log "building images keeper/*:$TAG"
for app in "${APPS[@]}"; do
  docker build -q --build-arg SERVICE="$app" --build-arg VERSION="$TAG" -t "keeper/$app:$TAG" . >/dev/null
done
docker build -q -t "keeper/scorer:$TAG" scorer >/dev/null
log "loading images into kind"
for app in "${APPS[@]}" scorer; do
  kind load docker-image --name "$CLUSTER" "keeper/$app:$TAG" >/dev/null
done

# Third-party images: load them from the local Docker cache when present, so
# the kind node does not download them again (Elasticsearch alone is ~1 GB).
for img in $(helm template keeper deploy/helm/keeper --set sftpgo.passiveIP=0.0.0.0 "$@" | awk '/^ *image: /{print $2}' | tr -d '"' | grep -v '^keeper/' | sort -u); do
  if docker image inspect "$img" >/dev/null 2>&1; then
    kind load docker-image --name "$CLUSTER" "$img" >/dev/null 2>&1 || true
  fi
done

# ── secrets ────────────────────────────────────────────────
kubectl --context "$CTX" get ns "$NS" >/dev/null 2>&1 || kubectl --context "$CTX" create ns "$NS" >/dev/null

pgpass=$(kubectl --context "$CTX" -n "$NS" get secret keeper-secrets -o jsonpath='{.data.POSTGRES_PASSWORD}' 2>/dev/null | base64 -d || true)
[[ -n "$pgpass" ]] || pgpass=$(openssl rand -hex 24)
# Encrypts the Lightroom tokens at rest; generated once and kept, so a
# reinstall does not log Lightroom out.
tokenkey=$(kubectl --context "$CTX" -n "$NS" get secret keeper-secrets -o jsonpath='{.data.KEEPER_TOKEN_KEY}' 2>/dev/null | base64 -d || true)
[[ -n "$tokenkey" ]] || tokenkey=$(openssl rand -base64 32)
log "writing secret keeper-secrets from .env"
kubectl --context "$CTX" -n "$NS" create secret generic keeper-secrets \
  --from-literal=DASH0_ENDPOINT="$(envval DASH0_ENDPOINT)" \
  --from-literal=DASH0_AUTH_TOKEN="$(envval DASH0_AUTH_TOKEN)" \
  --from-literal=DASH0_DATASET="$(envval DASH0_DATASET)" \
  --from-literal=DASH0_TRACE_URL="$(envval DASH0_TRACE_URL)" \
  --from-literal=CAMERA_FTP_PASSWORD="$(envval CAMERA_FTP_PASSWORD)" \
  --from-literal=POSTGRES_PASSWORD="$pgpass" \
  --from-literal=KEEPER_TOKEN_KEY="$tokenkey" \
  --from-literal=ADOBE_CLIENT_ID="$(envval ADOBE_CLIENT_ID)" \
  --from-literal=ADOBE_CLIENT_SECRET="$(envval ADOBE_CLIENT_SECRET)" \
  --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

# Self-signed certificate for FTPES and the console's https listener. Kept
# across runs so the camera sees the same certificate; RENEW_TLS=1 replaces it.
if [[ "${RENEW_TLS:-}" == 1 ]] || ! kubectl --context "$CTX" -n "$NS" get secret keeper-tls >/dev/null 2>&1; then
  log "creating self-signed certificate for $LAN_IP and localhost"
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 825 \
    -subj "/CN=keeper" -addext "subjectAltName=IP:$LAN_IP,IP:127.0.0.1,DNS:localhost" \
    -keyout "$tmp/tls.key" -out "$tmp/tls.crt" 2>/dev/null
  kubectl --context "$CTX" -n "$NS" create secret tls keeper-tls --cert="$tmp/tls.crt" --key="$tmp/tls.key" \
    --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
fi

# ── install ────────────────────────────────────────────────
log "installing chart (namespace $NS, LAN address $LAN_IP)"
helm upgrade --install keeper deploy/helm/keeper --kube-context "$CTX" -n "$NS" \
  --set image.tag="$TAG" --set sftpgo.passiveIP="$LAN_IP" "$@" --wait --timeout 6m >/dev/null

kubectl --context "$CTX" -n "$NS" get pods
cat <<EOF

Console:  http://$LAN_IP:8090   (also https://localhost:8443, self-signed)
Camera:   FTPES $LAN_IP:2121, user camera, password from .env (CAMERA_FTP_PASSWORD)
          explicit TLS, passive mode; set the camera to ignore the certificate
EOF
