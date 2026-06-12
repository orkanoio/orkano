#!/usr/bin/env bash
# Substrate smoke for the M1.2 build lane (the verdict shapes M1.6's E2E harness).
# Proves, on the CI substrate (kind), the claims the build pipeline rests on:
#   1. the orkano-buildkit AppArmor profile loaded on the HOST kernel reaches
#      pods inside kind nodes (kind nodes share the host kernel),
#   2. rootless BuildKit builds a git-URL context and pushes, admitted at PSA
#      baseline with the spike-F2 securityContext (ADR-0012),
#   3. NetworkPolicy is actually enforced, capability-probed in both directions:
#      probe pods under deny-all, then the build job itself with the egress
#      allowlist removed (INV-02),
#   4. the product registry manifests (config/registry/) admit under
#      orkano-system's restricted PSA, serve TLS from the cluster-internal CA,
#      and a test pod TLS-pushes + pulls with the projected CA bundle — and
#      cannot without it (M1.2 registry acceptance; registry.insecure never
#      ships, ADR-0012).
# Probe numbering: probe 1 is the prerequisite control (baseline connectivity
# before any policy — guards probe 2 against a broken cluster passing as
# "enforced"); probe 3 proves claims 1+2; probes 2+4 prove claim 3; probe 5
# proves claim 4 (its two TLS legs are the both-directions capability probe).
# Runs in CI (Linux, sudo) and locally (macOS + colima: the profile loads inside
# the colima VM, whose kernel the kind node containers share).
# Local teardown: kind delete cluster --name orkano-substrate-smoke
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DIR/../../.." && pwd)"
PROFILE="$REPO_ROOT/config/apparmor/orkano-buildkit.profile"
CLUSTER="${SMOKE_CLUSTER:-orkano-substrate-smoke}"
NODE_IMAGE="${SMOKE_NODE_IMAGE:-}"
BUILD_NS=orkano-smoke-build
INFRA_NS=orkano-smoke-infra
# Pinned cert-manager for the product-registry phase; the sha256 is of the
# release-asset cert-manager.yaml, checked before apply (supply-chain duty).
CERT_MANAGER_VERSION=v1.20.2
CERT_MANAGER_SHA256=1ce11cae912adecc69e6bb623435fafc9ed21505f9efff98bd71d7b80f01db1f

log() { printf '\n== %s\n' "$*"; }

dump_state() {
  set +e
  echo '--- dump: pods'
  kubectl get pods -A -o wide
  echo '--- dump: networkpolicies'
  kubectl get networkpolicy -n "$BUILD_NS" -o yaml
  echo '--- dump: jobs'
  kubectl describe job -n "$BUILD_NS"
  echo '--- dump: events (build ns)'
  kubectl get events -n "$BUILD_NS" --sort-by=.lastTimestamp | tail -40
  # Infra events too: an ImagePullBackOff on the registry (Docker Hub rate
  # limit) lives here, and is otherwise invisible in the CI log.
  echo '--- dump: events (infra ns)'
  kubectl get events -n "$INFRA_NS" --sort-by=.lastTimestamp | tail -20
  echo '--- dump: registry'
  kubectl describe deploy/registry -n "$INFRA_NS"
  echo '--- dump: job logs'
  for j in buildkit-smoke buildkit-smoke-denied; do
    echo "--- $j:"
    kubectl logs "job/$j" -n "$BUILD_NS" --tail=40 2>/dev/null
  done
  echo '--- dump: product registry (orkano-system)'
  kubectl get pods,certificate -n orkano-system -o wide
  kubectl get events -n orkano-system --sort-by=.lastTimestamp | tail -20
  echo '--- dump: registry-tls-probe logs'
  kubectl logs registry-tls-probe -n orkano-builds --tail=20 2>/dev/null
  set -e
}

fatal() {
  printf 'FATAL: %s\n' "$*" >&2
  dump_state >&2
  trap - ERR
  exit 1
}

connect_ok() {
  kubectl exec probe-client -n "$BUILD_NS" -- \
    timeout 15 wget -qO- -T 5 "http://probe-server.$BUILD_NS.svc.cluster.local:8080" >/dev/null 2>&1
}

# kubectl wait can only watch one condition; a failed job would block a
# wait-for-complete until its full timeout, so poll both terminal conditions.
job_outcome() {
  local name=$1 deadline=$(( $(date +%s) + $2 )) c f
  while [ "$(date +%s)" -lt "$deadline" ]; do
    c=$(kubectl get job "$name" -n "$BUILD_NS" -o 'jsonpath={.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null || true)
    f=$(kubectl get job "$name" -n "$BUILD_NS" -o 'jsonpath={.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || true)
    [ "$c" = "True" ] && { echo complete; return; }
    [ "$f" = "True" ] && { echo failed; return; }
    sleep 5
  done
  echo timeout
}

apply_job() {
  if [ -n "${SMOKE_GIT_CONTEXT:-}" ]; then
    sed "s|--opt=context=.*|--opt=context=${SMOKE_GIT_CONTEXT}|" "$1" | kubectl apply -f -
  else
    kubectl apply -f "$1"
  fi
}

for bin in kind kubectl docker curl; do
  command -v "$bin" >/dev/null || { printf 'FATAL: %s not found\n' "$bin" >&2; exit 1; }
done

log "load AppArmor profile orkano-buildkit on the host kernel"
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

KCFG="$(mktemp)"
export KUBECONFIG="$KCFG"
trap 'rm -f "$KCFG"' EXIT
trap 'dump_state >&2' ERR

# Reuse is only safe if the node carries both AppArmor mounts from
# kind-config.yaml; a cluster created without them can never enforce AppArmor.
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  if docker exec "$CLUSTER-control-plane" sh -c 'test -d /sys/kernel/security/apparmor && test -e /sbin/apparmor_parser' 2>/dev/null; then
    log "reuse kind cluster $CLUSTER"
    kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KCFG"
  else
    log "recreate kind cluster $CLUSTER (node lacks the securityfs mount)"
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

log "apply namespaces, registry, probe pods (idempotent re-run: clear old state)"
kubectl apply -f "$DIR/00-namespaces.yaml"
kubectl delete -f "$DIR/03-deny-all.yaml" -f "$DIR/04-egress-allowlist.yaml" --ignore-not-found
kubectl delete pod probe-server probe-client -n "$BUILD_NS" --ignore-not-found --grace-period=1
kubectl delete job buildkit-smoke buildkit-smoke-denied -n "$BUILD_NS" --ignore-not-found
kubectl apply -f "$DIR/01-registry.yaml" -f "$DIR/02-netpol-probe.yaml"
kubectl wait --for=condition=Ready pod/probe-server pod/probe-client -n "$BUILD_NS" --timeout=300s
kubectl wait --for=condition=Available deploy/registry -n "$INFRA_NS" --timeout=300s

log "probe 1: baseline connectivity (no policies — must succeed)"
ok=""
for _ in 1 2 3 4 5 6; do
  if connect_ok; then ok=yes; break; fi
  sleep 5
done
[ -n "$ok" ] || fatal "no baseline pod-to-pod connectivity; cluster is broken before any policy"
echo "OK: baseline connectivity"

log "probe 2: deny-all must actually block (capability probe, never config reads)"
kubectl apply -f "$DIR/03-deny-all.yaml"
blocked=""
for _ in 1 2 3 4 5 6; do
  sleep 5
  if ! connect_ok; then blocked=yes; break; fi
done
[ -n "$blocked" ] || fatal "deny-all NOT enforced — this CNI cannot carry the build-isolation invariants (kind-vs-k3d verdict input)"
echo "OK: deny-all enforced"

log "probe 3: egress allowlist + rootless BuildKit git-URL build (allow leg)"
kubectl apply -f "$DIR/04-egress-allowlist.yaml"
apply_job "$DIR/05-buildkit-job.yaml"
outcome="$(job_outcome buildkit-smoke 600)"
[ "$outcome" = "complete" ] || fatal "allow-leg build $outcome (expected complete)"
kubectl logs job/buildkit-smoke -n "$BUILD_NS" --tail=5
echo "OK: build completed under PSA baseline + orkano-buildkit AppArmor profile"

log "verify pushed artifact from the policy-free infra namespace"
# Not `kubectl run -i --rm`: its attach races the short-lived container and
# loses the output. Run to completion, then read the logs. Poll both terminal
# phases (same reason as job_outcome): a wait-for-Succeeded blocks 120s on a
# Failed pod and surfaces a generic timeout instead of the real error.
kubectl delete pod reg-check -n "$INFRA_NS" --ignore-not-found --grace-period=1
kubectl run reg-check -n "$INFRA_NS" --image=busybox:1.37 --restart=Never -- \
  wget -qO- -T 5 "http://registry.$INFRA_NS.svc.cluster.local:5000/v2/smoke/tags/list"
phase=""
deadline=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  phase=$(kubectl get pod reg-check -n "$INFRA_NS" -o 'jsonpath={.status.phase}' 2>/dev/null || true)
  if [ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ]; then break; fi
  sleep 3
done
[ "$phase" = "Succeeded" ] || fatal "reg-check pod phase=${phase:-unknown} — registry catalog query failed"
kubectl logs reg-check -n "$INFRA_NS" | grep -q '"fixture"' || fatal "image tag missing from registry catalog"
kubectl delete pod reg-check -n "$INFRA_NS" --grace-period=1
echo "OK: image pushed and listed"

log "probe 4: INV-02 deny leg — identical job without the allowlist must fail"
kubectl delete -f "$DIR/04-egress-allowlist.yaml"
sleep 3
apply_job "$DIR/06-buildkit-job-denied.yaml"
outcome="$(job_outcome buildkit-smoke-denied 240)"
[ "$outcome" = "failed" ] || fatal "deny-leg build $outcome (expected failed) — egress policy does not constrain build pods"
echo "OK: egress denial constrains the build pod"

log "install cert-manager $CERT_MANAGER_VERSION (pinned + checksummed)"
CM_YAML="$(mktemp)"
curl -fsSLo "$CM_YAML" "https://github.com/cert-manager/cert-manager/releases/download/$CERT_MANAGER_VERSION/cert-manager.yaml"
if command -v sha256sum >/dev/null; then
  echo "$CERT_MANAGER_SHA256  $CM_YAML" | sha256sum -c - >/dev/null
else
  echo "$CERT_MANAGER_SHA256  $CM_YAML" | shasum -a 256 -c - >/dev/null
fi
kubectl apply -f "$CM_YAML" >/dev/null
rm -f "$CM_YAML"
kubectl wait deploy --all -n cert-manager --for=condition=Available --timeout=300s

log "apply product namespaces + config/registry (restricted PSA enforced for real)"
kubectl apply -f "$REPO_ROOT/config/namespaces/namespaces.yaml"
# The first CR apply races cert-manager's webhook reaching serving readiness;
# Available on the deploy is not that. Retry instead of sleeping.
applied=""
apply_err="$(mktemp)"
for _ in $(seq 1 24); do
  if kubectl apply -f "$REPO_ROOT/config/registry/" 2>"$apply_err"; then applied=yes; break; fi
  sleep 5
done
[ -n "$applied" ] || { cat "$apply_err" >&2; fatal "config/registry did not apply (cert-manager webhook never became ready?)"; }
rm -f "$apply_err"
kubectl wait certificate orkano-registry-tls -n orkano-system --for=condition=Ready --timeout=180s
kubectl rollout status deploy/orkano-registry -n orkano-system --timeout=300s

log "publish the CA bundle for build-pod projection (init owns this copy at install time)"
ca_tmp="$(mktemp)"
kubectl get secret orkano-registry-tls -n orkano-system -o 'jsonpath={.data.ca\.crt}' | base64 --decode > "$ca_tmp"
[ -s "$ca_tmp" ] || fatal "orkano-registry-tls secret carries no ca.crt to publish"
kubectl create configmap orkano-registry-ca -n orkano-builds --from-file=ca.crt="$ca_tmp" \
  --dry-run=client -o yaml | kubectl apply -f -
rm -f "$ca_tmp"

log "probe 5: TLS push + pull from a test pod (must fail without the CA, succeed with it)"
kubectl delete pod registry-tls-probe -n orkano-builds --ignore-not-found --grace-period=1
kubectl apply -f "$DIR/07-registry-tls-probe.yaml"
phase=""
deadline=$(( $(date +%s) + 240 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  phase=$(kubectl get pod registry-tls-probe -n orkano-builds -o 'jsonpath={.status.phase}' 2>/dev/null || true)
  if [ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ]; then break; fi
  sleep 3
done
[ "$phase" = "Succeeded" ] || fatal "registry-tls-probe phase=${phase:-unknown} — TLS push/pull acceptance failed"
kubectl logs registry-tls-probe -n orkano-builds | grep -q 'OK: tls-verified push+pull' \
  || fatal "registry-tls-probe succeeded but the OK line is missing from its logs"
kubectl logs registry-tls-probe -n orkano-builds | tail -3
kubectl delete pod registry-tls-probe -n orkano-builds --grace-period=1
echo "OK: product registry serves TLS from the internal CA; projected bundle verified both ways"

log "PASS — substrate facts"
echo "cluster: $CLUSTER ($(kind version))"
echo "node image: $(kubectl get nodes -o 'jsonpath={.items[0].status.nodeInfo.kubeletVersion} {.items[0].status.nodeInfo.containerRuntimeVersion} {.items[0].status.nodeInfo.kernelVersion}')"
echo "cni pods: $(kubectl get pods -n kube-system -o name | grep -E 'kindnet|calico|cilium|cni' | tr '\n' ' ')"
