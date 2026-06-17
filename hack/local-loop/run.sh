#!/usr/bin/env bash
# Local event-path loop for M1.3 (the skeleton M1.6's hermetic E2E grows from).
# Wires the whole "doorbell → Build" path on a developer's machine and proves it
# end to end with one signed push:
#
#   curl (signed push) ─▶ receiver ─▶ Postgres queue ─▶ operator dispatcher
#                          ─▶ stub GitHub API (commit re-fetch) ─▶ Build CR
#
# What runs where, and why:
#   * kind          — the cluster the operator reconciles against (Apps/Builds).
#   * Postgres      — a throwaway host container (the platform queue DB).
#   * github-stub   — a host process standing in for the GitHub REST API the
#                     dispatcher re-fetches commits from (INV-04: the payload is
#                     never trusted, so a stub that returns a canned HEAD is a
#                     faithful stand-in for the only thing that matters here).
#   * operator      — run as a LOCAL binary against the cluster, not in-cluster:
#                     the operator/receiver Deployment manifests are M1.5's job
#                     and don't exist yet, and a local binary is the fast inner
#                     loop this target is named for. M1.6 swaps these for
#                     CI-built in-cluster images and asserts the rollout too.
#   * receiver      — same, a local binary on 127.0.0.1 we curl.
#
# Acceptance: a Build CR named "<app>-<sha[:12]>" appears. The loop deliberately
# stops there — the real BuildKit build + rollout needs the registry, the
# AppArmor profile, and cert-manager (the substrate-smoke's job), and is M1.6.
#
# Env knobs: KEEP=1 leaves everything up after the assertion (curl more events
# from another terminal; Ctrl-C tears it all down); CLEAN=1 also deletes the
# kind cluster on exit (default reuses it for fast re-runs); LOOP_CLUSTER,
# PG_PORT, RECEIVER_ADDR, STUB_ADDR override the defaults.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DIR/../.." && pwd)"

CLUSTER="${LOOP_CLUSTER:-orkano-local-loop}"
PG_CONTAINER=orkano-local-loop-pg
PG_IMAGE="postgres:17-alpine@sha256:979c4379dd698aba0b890599a6104e082035f98ef31d9b9291ec22f2b13059ca"
# A high, non-standard host port so a developer's local Postgres on 5432 (common
# on macOS) doesn't collide; nothing external connects — the loop builds its DSN.
PG_PORT="${PG_PORT:-55432}"
SUPER_DSN="postgres://orkano:orkano-test@127.0.0.1:${PG_PORT}/orkano?sslmode=disable"

RECEIVER_ADDR="${RECEIVER_ADDR:-127.0.0.1:8080}"
STUB_ADDR="${STUB_ADDR:-127.0.0.1:9099}"

WEBHOOK_SECRET="local-loop-secret"
APP_NAME=localloop-demo
APP_REPO="orkanoio/orkano-localloop"            # must match app.yaml + the body below
CANNED_SHA="a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
BUILD_NAME="${APP_NAME}-${CANNED_SHA:0:12}"
DELIVERY_ID="local-loop-delivery-0001"

TMP="$(mktemp -d)"
OPERATOR_PID="" RECEIVER_PID="" STUB_PID=""

log()   { printf '\n== %s\n' "$*"; }
fatal() { printf 'FATAL: %s\n' "$*" >&2; dump_state >&2; exit 1; }

dump_state() {
  set +e
  echo '--- operator log (tail) ---';  tail -n 40 "$TMP/operator.log" 2>/dev/null
  echo '--- receiver log (tail) ---';  tail -n 20 "$TMP/receiver.log" 2>/dev/null
  echo '--- github-stub log (tail) ---'; tail -n 20 "$TMP/stub.log" 2>/dev/null
  echo '--- apps / builds ---';   kubectl get apps,builds -n orkano-apps 2>/dev/null
  echo '--- jobs (builds ns) ---'; kubectl get jobs -n orkano-builds 2>/dev/null
  echo '--- queue rows ---';      docker exec "$PG_CONTAINER" psql -U orkano -d orkano \
    -c 'SELECT id, delivery_id, repo, event_type FROM webhook_deliveries;' 2>/dev/null
  set -e
}

cleanup() {
  set +e
  trap - EXIT INT TERM
  log "cleanup"
  for pid in "$OPERATOR_PID" "$RECEIVER_PID" "$STUB_PID"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
  wait 2>/dev/null
  docker rm -f "$PG_CONTAINER" >/dev/null 2>&1
  if [ "${CLEAN:-}" = "1" ]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  fi
  rm -rf "$TMP"
}

# Block until a URL answers (the receiver's /readyz also pings the DB).
wait_http() {
  local url=$1 deadline=$(( $(date +%s) + ${2:-60} ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    curl -fsS -o /dev/null "$url" 2>/dev/null && return 0
    sleep 1
  done
  return 1
}

for bin in go docker kind kubectl curl; do
  command -v "$bin" >/dev/null || { printf 'FATAL: %s not found\n' "$bin" >&2; exit 1; }
done

trap cleanup EXIT
trap 'exit 130' INT TERM

log "build operator, receiver, and the loop helper"
( cd "$REPO_ROOT" && go build -o "$TMP/operator" ./operator )
( cd "$REPO_ROOT" && go build -o "$TMP/receiver" ./receiver )
( cd "$REPO_ROOT" && go build -o "$TMP/helper"   ./hack/local-loop )

export KUBECONFIG="$TMP/kubeconfig"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "reuse kind cluster $CLUSTER"
  kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
else
  log "create kind cluster $CLUSTER"
  kind create cluster --name "$CLUSTER" --wait 120s --kubeconfig "$KUBECONFIG"
fi

log "start Postgres ($PG_CONTAINER on 127.0.0.1:$PG_PORT)"
docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$PG_CONTAINER" \
  -e POSTGRES_USER=orkano -e POSTGRES_PASSWORD=orkano-test -e POSTGRES_DB=orkano \
  -p "127.0.0.1:${PG_PORT}:5432" "$PG_IMAGE" >/dev/null
ready=""
for _ in $(seq 1 30); do
  if docker exec "$PG_CONTAINER" pg_isready -U orkano -d orkano >/dev/null 2>&1; then ready=yes; break; fi
  sleep 1
done
[ -n "$ready" ] || fatal "Postgres never became ready"

log "apply migrations + role passwords; capture least-privilege DSNs"
dsns="$("$TMP/helper" migrate --dsn "$SUPER_DSN")"
RECEIVER_DSN="$(printf '%s\n' "$dsns" | sed -n 's/^RECEIVER_DSN=//p')"
DISPATCHER_DSN="$(printf '%s\n' "$dsns" | sed -n 's/^DISPATCHER_DSN=//p')"
[ -n "$RECEIVER_DSN" ] && [ -n "$DISPATCHER_DSN" ] || fatal "migrate did not print both DSNs"

log "install CRDs, namespaces, service accounts"
kubectl apply -f "$REPO_ROOT/config/crd/" >/dev/null
# The operator's Domain/RegistryCert reconcilers watch cert-manager Certificates;
# the test-only CRD lets the manager start without a full cert-manager install.
kubectl apply -f "$REPO_ROOT/hack/testdata/crds/cert-manager.io_certificates.yaml" >/dev/null
kubectl apply -f "$REPO_ROOT/config/namespaces/namespaces.yaml" >/dev/null
# Lets the Build controller's Job pods be admitted (they name the orkano-build SA).
kubectl apply -f "$REPO_ROOT/config/rbac/serviceaccounts.yaml" >/dev/null
kubectl wait --for=condition=Established crd/apps.orkano.io crd/builds.orkano.io crd/domains.orkano.io --timeout=60s >/dev/null

log "create the GitHub App secret (throwaway key) and the App CR"
"$TMP/helper" genkey > "$TMP/app-key.pem"
kubectl create secret generic orkano-github-app -n orkano-system \
  --from-literal=app-id=1 --from-file=private-key.pem="$TMP/app-key.pem" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f "$DIR/app.yaml" >/dev/null
# Reset the assertion target: the Build name is deterministic (<app>-<sha[:12]>)
# and a reused cluster keeps the prior run's Build (cleanup removes processes +
# the DB, never cluster objects). Delete it BEFORE firing the event — nothing
# creates a Build until the delivery is enqueued — so the poll below passes only
# if THIS run's dispatcher recreated it (the queue is fresh, so it re-enqueues).
kubectl delete build "$BUILD_NAME" -n orkano-apps --ignore-not-found >/dev/null 2>&1 || true

log "start github-stub on $STUB_ADDR"
"$TMP/helper" github-stub --addr "$STUB_ADDR" --sha "$CANNED_SHA" >"$TMP/stub.log" 2>&1 &
STUB_PID=$!
wait_http "http://$STUB_ADDR/repos/orkanoio/orkano-localloop" 15 \
  || fatal "github-stub did not come up (see $TMP/stub.log)"

log "start operator (dispatcher → stub GitHub) against the cluster"
ORKANO_DB_DSN="$DISPATCHER_DSN" "$TMP/operator" \
  --github-base-url="http://$STUB_ADDR" \
  --dispatch-poll-interval=2s \
  --zap-log-level=info >"$TMP/operator.log" 2>&1 &
OPERATOR_PID=$!

log "start receiver on $RECEIVER_ADDR"
ORKANO_WEBHOOK_SECRET="$WEBHOOK_SECRET" \
ORKANO_DB_DSN="$RECEIVER_DSN" \
ORKANO_REPO_ALLOWLIST="$APP_REPO" \
ORKANO_ADDR="$RECEIVER_ADDR" "$TMP/receiver" >"$TMP/receiver.log" 2>&1 &
RECEIVER_PID=$!
wait_http "http://$RECEIVER_ADDR/readyz" 30 \
  || fatal "receiver did not become ready (see $TMP/receiver.log)"

log "fire one signed push event"
printf '%s' "{\"repository\":{\"full_name\":\"$APP_REPO\"}}" > "$TMP/body.json"
SIG="$("$TMP/helper" sign --secret "$WEBHOOK_SECRET" < "$TMP/body.json")"
code="$(curl -sS -o /dev/null -w '%{http_code}' \
  -H 'X-GitHub-Event: push' \
  -H "X-GitHub-Delivery: $DELIVERY_ID" \
  -H "X-Hub-Signature-256: $SIG" \
  -H 'Content-Type: application/json' \
  --data-binary @"$TMP/body.json" \
  "http://$RECEIVER_ADDR/webhook")"
[ "$code" = "202" ] || fatal "receiver answered $code (expected 202)"
echo "OK: receiver accepted the signed push (202)"

log "wait for the dispatcher to create Build $BUILD_NAME"
found=""
for _ in $(seq 1 45); do
  if kubectl get build "$BUILD_NAME" -n orkano-apps >/dev/null 2>&1; then found=yes; break; fi
  sleep 2
done
[ -n "$found" ] || fatal "Build $BUILD_NAME never appeared — the event path is broken"

log "PASS — the signed push produced a Build CR"
kubectl get build "$BUILD_NAME" -n orkano-apps -o wide
echo
echo "Build commit:   $(kubectl get build "$BUILD_NAME" -n orkano-apps -o jsonpath='{.spec.commit}')"
echo "Build app:      $(kubectl get build "$BUILD_NAME" -n orkano-apps -o jsonpath='{.spec.appName}')"
echo "(the real BuildKit build + rollout is M1.6; this loop proves the event path)"

if [ "${KEEP:-}" = "1" ]; then
  cat <<EOF

Environment is up. Fire more events from another terminal, e.g.:

  body='{"repository":{"full_name":"$APP_REPO"}}'
  sig="\$(printf '%s' "\$body" | $TMP/helper sign --secret $WEBHOOK_SECRET)"
  curl -i -H 'X-GitHub-Event: push' -H "X-GitHub-Delivery: \$RANDOM" \\
    -H "X-Hub-Signature-256: \$sig" --data-binary "\$body" \\
    http://$RECEIVER_ADDR/webhook

Logs: $TMP/{operator,receiver,stub}.log   ·   kubeconfig: $KUBECONFIG
Press Ctrl-C to tear everything down.
EOF
  wait "$OPERATOR_PID" "$RECEIVER_PID" "$STUB_PID"
fi
