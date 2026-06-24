#!/usr/bin/env bash
# Hermetic engine E2E for Phase 1 (M1.6). Grows from two seeds:
#   * hack/ci/substrate-smoke/ — the kind + AppArmor + registry + lockdown
#     substrate (this reuses its kind-config and its idioms), and
#   * hack/local-loop/ — the signed-push -> receiver -> queue -> dispatcher ->
#     Build event path (this runs the same flow, but in-cluster from CI-built
#     images and all the way through the real build + rollout).
#
# It stands up a full Orkano control plane on kind from locally-built operator,
# receiver, and helper images plus the real config/ manifests, an in-cluster
# GitHub API stub and git fixture (so nothing reaches github.com / api.github.com),
# then drives a signed push to a digest-pinned, HTTP-answering deployment. This
# file (Commit 3) brings the platform up and gates on every component Ready; the
# engine drive and the invariant probes are appended in the following commits.
#
# Runs in CI (Linux, sudo) and locally (macOS + colima: the AppArmor profile
# loads inside the colima VM whose kernel the kind nodes share).
# Local teardown: kind delete cluster --name orkano-e2e
#
# Env knobs: KEEP=1 leaves the cluster up after the run; CLEAN=1 deletes it on
# exit; E2E_CLUSTER / SMOKE_NODE_IMAGE override the cluster name / node image.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DIR/../../.." && pwd)"
# Run from the repo root so the relative `go build ./operator` paths resolve no
# matter where the script is invoked from (everything else uses absolute paths).
cd "$REPO_ROOT"
PROFILE="$REPO_ROOT/config/apparmor/orkano-buildkit.profile"
CLUSTER="${E2E_CLUSTER:-orkano-e2e}"
NODE_IMAGE="${SMOKE_NODE_IMAGE:-}"

# The hermetic fixture identity, shared by the App CR, the receiver allowlist,
# the github-stub, and the git fixture repo path.
REPO="orkanoio/orkano-e2e"
APP_NAME="e2e-web"
WEBHOOK_SECRET="orkano-e2e-webhook-secret"          # mirrors 10-secrets.yaml
GITFIXTURE_REPO="/srv/git/${REPO}.git"

OPERATOR_IMG="orkano-operator:e2e"
RECEIVER_IMG="orkano-receiver:e2e"
HELPER_IMG="orkano-e2e-helper:e2e"

# The registry host every image ref carries (portless = 443); the node-wiring
# maps it to the registry ClusterIP so the kubelet can pull the App image.
REGISTRY_HOST="orkano-registry.orkano-system.svc.cluster.local"

TMP="$(mktemp -d)"
PF_PID=""   # current kubectl port-forward, killed by cleanup

log() { printf '\n== %s\n' "$*"; }

dump_state() {
  set +e
  echo '--- dump: pods (all namespaces)'
  kubectl get pods -A -o wide
  echo '--- dump: events (orkano-system)'
  kubectl get events -n orkano-system --sort-by=.lastTimestamp | tail -30
  echo '--- dump: events (orkano-e2e)'
  kubectl get events -n orkano-e2e --sort-by=.lastTimestamp | tail -15
  echo '--- dump: deployments / statefulsets'
  kubectl get deploy,statefulset -A | grep -E 'orkano|cert-manager|NAME'
  echo '--- dump: migration job'
  kubectl describe job orkano-migrate -n orkano-system 2>/dev/null
  kubectl logs job/orkano-migrate -n orkano-system --tail=30 2>/dev/null
  echo '--- dump: operator log'
  kubectl logs deploy/orkano-operator -n orkano-system --tail=40 2>/dev/null
  echo '--- dump: receiver log'
  kubectl logs deploy/orkano-receiver -n orkano-system --tail=20 2>/dev/null
  echo '--- dump: gitfixture / github-stub logs'
  kubectl logs deploy/gitfixture -n orkano-e2e --tail=15 2>/dev/null
  kubectl logs deploy/github-stub -n orkano-e2e --tail=15 2>/dev/null
  echo '--- dump: registry'
  kubectl get pods,certificate -n orkano-system -l app.kubernetes.io/name=orkano-registry -o wide 2>/dev/null
  set -e
}

fatal() {
  printf 'FATAL: %s\n' "$*" >&2
  dump_state >&2
  trap - ERR
  exit 1
}

cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  rm -rf "$TMP"
  [ "${CLEAN:-}" = "1" ] && kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  return 0   # never mask the script's exit code
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

# wire_registry_pull is the kind analog of internal/install/registry.go: the App
# Deployment pulls orkano-registry.orkano-system.svc.cluster.local/<app>@sha256:…,
# but a kind node's containerd resolves neither cluster DNS nor the internal CA.
# So map the registry FQDN -> its ClusterIP in each node's /etc/hosts and trust
# the internal CA via a containerd registry-host config (kind sets
# config_path=/etc/containerd/certs.d), then render the per-node ingress allow
# init owns in prod. The build PUSH already works (in-pod DNS + buildkitd.toml CA);
# this is only the kubelet PULL side.
wire_registry_pull() {
  local clusterip ca node node_ips ip
  clusterip="$(kubectl get svc orkano-registry -n orkano-system -o 'jsonpath={.spec.clusterIP}')"
  [ -n "$clusterip" ] || fatal "orkano-registry Service has no ClusterIP"
  ca="$TMP/registry-ca.crt"
  kubectl get secret orkano-registry-tls -n orkano-system -o 'jsonpath={.data.ca\.crt}' | base64 --decode > "$ca"
  [ -s "$ca" ] || fatal "orkano-registry-tls carries no ca.crt"

  for node in "$CLUSTER-control-plane" "$CLUSTER-worker"; do
    docker exec "$node" mkdir -p "/etc/containerd/certs.d/$REGISTRY_HOST"
    docker cp "$ca" "$node:/etc/containerd/certs.d/$REGISTRY_HOST/ca.crt"
    docker exec -i "$node" sh -c "cat > /etc/containerd/certs.d/$REGISTRY_HOST/hosts.toml" <<EOF
server = "https://$REGISTRY_HOST"

[host."https://$REGISTRY_HOST"]
  capabilities = ["pull", "resolve"]
  ca = "/etc/containerd/certs.d/$REGISTRY_HOST/ca.crt"
EOF
    # Replace-in-place (not append): a reused cluster must not keep a stale
    # mapping if the registry ClusterIP ever changed (mirrors registry.go).
    docker exec -i "$node" sh -c "
      grep -v ' $REGISTRY_HOST\$' /etc/hosts > /etc/hosts.new || true
      echo '$clusterip $REGISTRY_HOST' >> /etc/hosts.new
      cat /etc/hosts.new > /etc/hosts
      rm -f /etc/hosts.new
    "
    # Without config_path the certs.d files are silently ignored and the pull
    # x509-fails; fatal here (diagnosable) rather than as an opaque 4-min later
    # ImagePullBackOff. kind v0.32 sets it by default.
    docker exec "$node" grep -q '/etc/containerd/certs.d' /etc/containerd/config.toml \
      || fatal "$node containerd has no registry config_path=/etc/containerd/certs.d (kind regression or non-default node image)"
    echo "  $node: registry node-wiring written (certs.d + /etc/hosts)"
  done

  # The cross-node kubelet-pull allow init renders at install (M1.5, smoke probe
  # 9). Same-node pulls are CNI-exempt, but rendering it removes any doubt and
  # mirrors production; node IPs are install-specific so it can't be a static file.
  node_ips="$(kubectl get nodes -o 'jsonpath={.items[*].status.addresses[?(@.type=="InternalIP")].address}')"
  [ -n "$node_ips" ] || fatal "no node InternalIPs found"
  {
    cat <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: orkano-registry-ingress-nodes
  namespace: orkano-system
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: orkano-registry
  policyTypes: [Ingress]
  ingress:
    - ports:
        - port: 5000
          protocol: TCP
      from:
EOF
    for ip in $node_ips; do
      printf '        - ipBlock:\n            cidr: %s/32\n' "$ip"
    done
  } | kubectl apply -f - >/dev/null
}

# Poll a Job's two terminal conditions (a failed Job would otherwise block a
# wait-for-complete its whole timeout). From substrate-smoke.
job_outcome() {
  local name=$1 deadline=$(( $(date +%s) + $2 )) ns=${3:-orkano-system} c f
  while [ "$(date +%s)" -lt "$deadline" ]; do
    c=$(kubectl get job "$name" -n "$ns" -o 'jsonpath={.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null || true)
    f=$(kubectl get job "$name" -n "$ns" -o 'jsonpath={.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || true)
    [ "$c" = "True" ] && { echo complete; return; }
    [ "$f" = "True" ] && { echo failed; return; }
    sleep 5
  done
  echo timeout
}

# kubectl apply with a retry, for the window where cert-manager's webhook is
# Available but not yet serving (the first CR apply races it). From substrate-smoke.
retry_apply() {
  local path=$1 tries=${2:-24} err
  err="$(mktemp)"
  for _ in $(seq 1 "$tries"); do
    if kubectl apply -f "$path" 2>"$err"; then rm -f "$err"; return 0; fi
    sleep 5
  done
  cat "$err" >&2
  rm -f "$err"
  return 1
}

build_load() {
  # $1 = go package, $2 = output binary name, $3 = image tag
  local pkg=$1 bin=$2 tag=$3 ctx
  ctx="$(mktemp -d)"
  GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$ctx/$bin" "$pkg"
  docker build -q -f "$REPO_ROOT/build/Dockerfile" --build-arg BINARY="$bin" -t "$tag" "$ctx" >/dev/null
  kind load docker-image "$tag" --name "$CLUSTER" >/dev/null
  rm -rf "$ctx"
}

pull_load() {
  for img in "$@"; do
    docker pull -q "$img" >/dev/null
    kind load docker-image "$img" --name "$CLUSTER" >/dev/null
  done
}

# extract_image prints the first image ref matching regex $1 in file $2, or
# empty (|| true keeps a no-match from tripping set -e/pipefail; the caller
# fatals on empty, so a format change surfaces rather than silently skipping).
extract_image() { grep -oE "$1" "$2" | head -1 || true; }

for bin in kind kubectl docker go git curl; do
  command -v "$bin" >/dev/null || { printf 'FATAL: %s not found\n' "$bin" >&2; exit 1; }
done
# After the tool check, so a missing `go` surfaces as the FATAL above, not an
# opaque exit from the command substitution.
ARCH="$(go env GOARCH)"

log "load AppArmor profile orkano-buildkit on the host kernel (build pods require it)"
case "$(uname -s)" in
  Linux)
    sudo install -m 0644 "$PROFILE" /etc/apparmor.d/orkano-buildkit
    sudo apparmor_parser -r /etc/apparmor.d/orkano-buildkit
    sudo grep -q '^orkano-buildkit ' /sys/kernel/security/apparmor/profiles \
      || { printf 'FATAL: profile not in the kernel after apparmor_parser\n' >&2; exit 1; }
    ;;
  Darwin)
    colima ssh -- sudo sh -c \
      'cat > /etc/apparmor.d/orkano-buildkit && apparmor_parser -r /etc/apparmor.d/orkano-buildkit' \
      < "$PROFILE"
    colima ssh -- sudo grep -q '^orkano-buildkit ' /sys/kernel/security/apparmor/profiles \
      || { printf 'FATAL: profile not in the colima VM kernel\n' >&2; exit 1; }
    ;;
  *)
    printf 'FATAL: unsupported host OS %s\n' "$(uname -s)" >&2; exit 1 ;;
esac

KCFG="$TMP/kubeconfig"
export KUBECONFIG="$KCFG"
trap cleanup EXIT
trap 'dump_state >&2' ERR

log "kind-config drift guard (must match the substrate smoke's AppArmor mounts)"
diff "$DIR/kind-config.yaml" "$REPO_ROOT/hack/ci/substrate-smoke/kind-config.yaml" >/dev/null \
  || fatal "hack/ci/e2e/kind-config.yaml drifted from the substrate-smoke kind-config; keep the AppArmor mounts in sync"

# Reuse only when both nodes carry the AppArmor mounts (a cluster created without
# them can never enforce the profile). Same logic as the substrate smoke.
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  reusable=yes
  for node in "$CLUSTER-control-plane" "$CLUSTER-worker"; do
    docker exec "$node" sh -c 'test -d /sys/kernel/security/apparmor && test -e /sbin/apparmor_parser' 2>/dev/null \
      || reusable=""
  done
  if [ -n "$reusable" ]; then
    log "reuse kind cluster $CLUSTER"
    kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KCFG"
  else
    log "recreate kind cluster $CLUSTER (missing worker node or AppArmor mount)"
    kind delete cluster --name "$CLUSTER"
  fi
fi
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "create kind cluster $CLUSTER"
  if [ -n "$NODE_IMAGE" ]; then
    kind create cluster --name "$CLUSTER" --config "$DIR/kind-config.yaml" --image "$NODE_IMAGE" --wait 180s --kubeconfig "$KCFG"
  else
    kind create cluster --name "$CLUSTER" --config "$DIR/kind-config.yaml" --wait 180s --kubeconfig "$KCFG"
  fi
fi

log "build + load operator, receiver, and the combined e2e helper images"
build_load ./operator orkano-operator "$OPERATOR_IMG"
build_load ./receiver orkano-receiver "$RECEIVER_IMG"
# The helper image bakes the git fixture repo (seed.sh) and ships the
# github-stub + git-fixture subcommands; its own context is assembled here.
hctx="$(mktemp -d)"
GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$hctx/local-loop-helper" ./hack/local-loop
cp "$DIR/gitfixture/seed.sh" "$hctx/seed.sh"
cp -a "$DIR/fixture" "$hctx/fixture"
docker build -q -f "$DIR/gitfixture/Dockerfile" -t "$HELPER_IMG" "$hctx" >/dev/null
kind load docker-image "$HELPER_IMG" --name "$CLUSTER" >/dev/null
rm -rf "$hctx"
# A host helper for genkey (the GitHub App key) and sign (the webhook signature).
go build -o "$TMP/helper" ./hack/local-loop

log "pre-pull + load the cluster's Docker Hub images (dodges the anonymous-pull limit)"
registry_img="$(extract_image 'registry:[0-9.]+@sha256:[0-9a-f]{64}' "$REPO_ROOT/config/registry/registry.yaml")"
postgres_img="$(extract_image 'postgres:[0-9]+@sha256:[0-9a-f]{64}' "$REPO_ROOT/config/components/platform-postgres.yaml")"
buildkit_img="$(extract_image 'moby/buildkit:[^"@ ]+@sha256:[0-9a-f]{64}' "$REPO_ROOT/operator/internal/buildjob/job.go")"
for v in "$registry_img" "$postgres_img" "$buildkit_img"; do
  [ -n "$v" ] || fatal "could not extract a pinned image; a manifest/const format change broke the pre-load"
done
pull_load "$registry_img" "$postgres_img" "$buildkit_img"

log "install CRDs"
kubectl apply -f "$REPO_ROOT/config/crd/" >/dev/null
kubectl wait --for=condition=Established \
  crd/apps.orkano.io crd/builds.orkano.io crd/domains.orkano.io crd/postgreses.orkano.io --timeout=60s >/dev/null

log "install cert-manager (vendored, digest-pinned) and wait for it to serve"
kubectl apply -f "$REPO_ROOT/config/cert-manager/" >/dev/null
kubectl wait deploy --all -n cert-manager --for=condition=Available --timeout=300s

log "apply namespaces, RBAC, buildkit config, and the lockdown NetworkPolicies"
kubectl apply -f "$REPO_ROOT/config/namespaces/namespaces.yaml" >/dev/null
kubectl apply -f "$DIR/manifests/00-namespace-e2e.yaml" >/dev/null
kubectl apply -f "$REPO_ROOT/config/rbac/" >/dev/null
kubectl apply -f "$REPO_ROOT/config/buildkit/" >/dev/null
kubectl apply -f "$REPO_ROOT/config/netpol/" >/dev/null
kubectl apply -f "$DIR/manifests/50-test-egress-gitfixture.yaml" >/dev/null

log "deploy the in-cluster registry (internal CA) and wait for its TLS cert"
retry_apply "$REPO_ROOT/config/registry/" || fatal "config/registry did not apply (cert-manager webhook never ready?)"
kubectl wait certificate orkano-registry-tls -n orkano-system --for=condition=Ready --timeout=180s
kubectl rollout status deploy/orkano-registry -n orkano-system --timeout=300s

log "publish the registry CA bundle for build-pod projection (init owns this in prod)"
ca_tmp="$TMP/ca.crt"
kubectl get secret orkano-registry-tls -n orkano-system -o 'jsonpath={.data.ca\.crt}' | base64 --decode > "$ca_tmp"
[ -s "$ca_tmp" ] || fatal "orkano-registry-tls carries no ca.crt"
kubectl create configmap orkano-registry-ca -n orkano-builds --from-file=ca.crt="$ca_tmp" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

log "apply the orkano-platform ClusterIssuer (self-signed, hermetic) and the platform Secrets"
# retry_apply: the ClusterIssuer is a cert-manager.io resource behind the
# validating webhook (failurePolicy Fail), same race the registry CRs guard against.
retry_apply "$DIR/manifests/45-platform-issuer.yaml" || fatal "orkano-platform ClusterIssuer did not apply"
kubectl apply -f "$DIR/manifests/10-secrets.yaml" >/dev/null
"$TMP/helper" genkey > "$TMP/app-key.pem"
kubectl create secret generic orkano-github-app -n orkano-system \
  --from-literal=app-id=1 --from-file=private-key.pem="$TMP/app-key.pem" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

log "deploy the platform Postgres and run the migration Job"
kubectl apply -f "$REPO_ROOT/config/components/platform-postgres.yaml" >/dev/null
# Longer than the pod's startupProbe window (60×5s = 300s for initdb on cold
# kind storage) so the rollout wait can't expire before the probe would.
kubectl rollout status statefulset/orkano-postgres -n orkano-system --timeout=420s
kubectl apply -f "$DIR/manifests/20-migration-job.yaml" >/dev/null
outcome="$(job_outcome orkano-migrate 240)"
[ "$outcome" = "complete" ] || fatal "migration Job $outcome (expected complete)"

log "deploy the operator and receiver"
kubectl apply -f "$DIR/manifests/30-operator.yaml" -f "$DIR/manifests/31-receiver.yaml" >/dev/null

log "deploy the git fixture, discover its HEAD SHA, and point the github-stub at it"
kubectl apply -f "$DIR/manifests/41-gitfixture.yaml" >/dev/null
kubectl rollout status deploy/gitfixture -n orkano-e2e --timeout=180s
FIXTURE_SHA="$(kubectl exec deploy/gitfixture -n orkano-e2e -- git -C "$GITFIXTURE_REPO" rev-parse HEAD)"
echo "$FIXTURE_SHA" | grep -qE '^[0-9a-f]{40}$' || fatal "git fixture HEAD is not a 40-hex SHA: '$FIXTURE_SHA'"
sed "s/__STUB_SHA__/$FIXTURE_SHA/" "$DIR/manifests/40-github-stub.yaml" | kubectl apply -f - >/dev/null

log "wait for every component to become Ready"
kubectl rollout status deploy/github-stub -n orkano-e2e --timeout=120s
kubectl rollout status deploy/orkano-operator -n orkano-system --timeout=180s
kubectl rollout status deploy/orkano-receiver -n orkano-system --timeout=180s

log "apply the App and Domain, then wire the nodes to pull from the in-cluster registry"
kubectl apply -f "$DIR/manifests/60-app.yaml" >/dev/null
wire_registry_pull

log "fire one signed push event at the receiver"
# A reused cluster keeps the prior run's Build (the name is deterministic); delete
# it first so the assertion proves THIS run's dispatcher recreated it.
BUILD_NAME="${APP_NAME}-${FIXTURE_SHA:0:12}"
kubectl delete build "$BUILD_NAME" -n orkano-apps --ignore-not-found >/dev/null 2>&1 || true
kubectl port-forward svc/orkano-receiver -n orkano-system 18080:80 >/dev/null 2>&1 &
PF_PID=$!
wait_http "http://127.0.0.1:18080/readyz" 60 || fatal "receiver port-forward never answered /readyz"
printf '%s' "{\"repository\":{\"full_name\":\"$REPO\"}}" > "$TMP/body.json"
SIG="$("$TMP/helper" sign --secret "$WEBHOOK_SECRET" < "$TMP/body.json")"
code="$(curl -sS -o /dev/null -w '%{http_code}' \
  -H 'X-GitHub-Event: push' \
  -H "X-GitHub-Delivery: e2e-$(date +%s)" \
  -H "X-Hub-Signature-256: $SIG" \
  -H 'Content-Type: application/json' \
  --data-binary @"$TMP/body.json" \
  http://127.0.0.1:18080/webhook)"
[ "$code" = "202" ] || fatal "receiver answered $code (expected 202)"
kill "$PF_PID" 2>/dev/null; PF_PID=""
echo "OK: receiver accepted the signed push (202)"

log "wait for the dispatcher to create Build $BUILD_NAME"
found=""
for _ in $(seq 1 60); do
  kubectl get build "$BUILD_NAME" -n orkano-apps >/dev/null 2>&1 && { found=yes; break; }
  sleep 2
done
[ -n "$found" ] || fatal "Build $BUILD_NAME never appeared — the event path is broken"

log "wait for the rootless BuildKit build to succeed and push a digest-pinned image"
phase=""
deadline=$(( $(date +%s) + 600 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  phase="$(kubectl get build "$BUILD_NAME" -n orkano-apps -o 'jsonpath={.status.phase}' 2>/dev/null || true)"
  [ "$phase" = "Succeeded" ] && break
  [ "$phase" = "Failed" ] && fatal "Build $BUILD_NAME Failed — see the build Job logs in the dump"
  sleep 5
done
[ "$phase" = "Succeeded" ] || fatal "Build $BUILD_NAME did not succeed (phase=${phase:-none})"
BUILD_IMG="$(kubectl get build "$BUILD_NAME" -n orkano-apps -o 'jsonpath={.status.image}')"
echo "$BUILD_IMG" | grep -q '@sha256:' || fatal "Build image is not digest-pinned: $BUILD_IMG"
echo "OK: build pushed $BUILD_IMG"

log "wait for the App to roll out to the digest and assert the running pod is digest-pinned"
# rollout status exits non-zero immediately on a not-yet-created Deployment, so
# poll for it first (the App controller creates it after observing the Build).
for _ in $(seq 1 30); do
  kubectl get deploy/"$APP_NAME" -n orkano-apps >/dev/null 2>&1 && break
  sleep 2
done
kubectl rollout status deploy/"$APP_NAME" -n orkano-apps --timeout=240s || fatal "App Deployment never rolled out"
POD_IMG="$(kubectl get pods -n orkano-apps -l app.orkano.io/app="$APP_NAME" -o 'jsonpath={.items[0].spec.containers[0].image}')"
echo "$POD_IMG" | grep -q '@sha256:' || fatal "running App pod image is not digest-pinned: $POD_IMG"
echo "OK: App pod runs $POD_IMG"

log "assert the App answers HTTP on the digest-pinned image"
kubectl port-forward deploy/"$APP_NAME" -n orkano-apps 18090:8080 >/dev/null 2>&1 &
PF_PID=$!
wait_http "http://127.0.0.1:18090/" 60 || fatal "App port-forward never answered after 60s"
body="$(curl -fsS "http://127.0.0.1:18090/" 2>/dev/null || true)"
kill "$PF_PID" 2>/dev/null; PF_PID=""
echo "$body" | grep -q 'orkano-e2e-ok' || fatal "App did not answer with the expected body (got: '$body')"
echo "OK: App answered HTTP with the fixture body"

log "assert the Domain leg (Certificate Ready + App.status.url)"
kubectl wait certificate "${APP_NAME}-example-test-tls" -n orkano-apps --for=condition=Ready --timeout=120s \
  || fatal "Domain Certificate never became Ready (self-signed orkano-platform issuer)"
APP_URL="$(kubectl get app "$APP_NAME" -n orkano-apps -o 'jsonpath={.status.url}')"
[ "$APP_URL" = "https://e2e-web.example.test" ] || fatal "App.status.url = '$APP_URL', want https://e2e-web.example.test"
echo "OK: App.status.url = $APP_URL"

log "PASS — engine E2E: signed push -> build -> digest-pinned rollout -> HTTP"
echo "cluster:        $CLUSTER ($(kind version))"
echo "fixture commit: $FIXTURE_SHA (repo $REPO, app $APP_NAME)"
echo "build image:    $BUILD_IMG"
echo "(the invariant probes are appended in the following commit)"
