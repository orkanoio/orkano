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
#      ships, ADR-0012),
#   5. the M1.2 lockdown manifests (config/netpol/) hold on the real
#      substrate: a build-labeled pod keeps DNS + registry + 443, everything
#      else in orkano-builds has nothing, the registry accepts ingress only
#      from build pods plus operator-labeled pods (the digest-resolution
#      leg, keyed on the label and not the namespace) — and the cross-node
#      kubelet-pull path works through the per-node /32 allow that init
#      renders at install (rehearsed here; kindnet blocks cross-node host
#      traffic without it — found empirically), while the kubelet's own
#      health probes keep passing,
#   6. the product build Job template (operator/internal/buildjob, golden
#      copy 09-build-job-template.yaml pinned by unit test) admits at PSA
#      baseline and builds + TLS-pushes a public repo under the full
#      lockdown — the M1.2 Job-template acceptance's end-to-end half (the
#      zero-warnings half lives in envtest),
#   7. the Static build strategy (golden 11-static-build-job-template.yaml):
#      an init container injects a generated COPY-only Dockerfile, which
#      buildkit reads via the dockerfilekey + --local opt while the git URL
#      stays the COPY context — the in-cluster confirmation of that
#      undocumented opt on the pinned rootless image, under the full lockdown.
# Probe numbering: probe 1 is the prerequisite control (baseline connectivity
# before any policy — guards probe 2 against a broken cluster passing as
# "enforced"); probe 3 proves claims 1+2; probes 2+4 prove claim 3; probe 5
# proves claim 4 (its two TLS legs are the both-directions capability probe);
# probe 6 is claim 5's no-policy controls (same pattern as probe 1), probe 7
# its deny legs — each isolating exactly ONE policy, and doubling as the
# CNI-propagation barrier so probe 8 cannot false-pass before the rules hit
# the kernel — probe 8 its allow legs (probe 5 re-run under the policies,
# then the operator-labeled canary with its unlabeled negative control),
# probe 9 its node-originated leg; probe 10 proves claim 6; probe 11 proves
# claim 7.
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
  kubectl get networkpolicy -A
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
  echo '--- dump: product template jobs (orkano-builds)'
  for j in template-smoke static-template-smoke; do
    kubectl describe job "$j" -n orkano-builds 2>/dev/null
    kubectl logs "job/$j" -n orkano-builds --tail=40 --all-containers 2>/dev/null
  done
  echo '--- dump: product registry (orkano-system)'
  kubectl get pods,certificate -n orkano-system -o wide
  kubectl get events -n orkano-system --sort-by=.lastTimestamp | tail -20
  echo '--- dump: registry-tls-probe logs'
  kubectl logs registry-tls-probe -n orkano-builds --tail=20 2>/dev/null
  echo '--- dump: lockdown probes (orkano-apps / orkano-builds / node probe)'
  kubectl get events -n orkano-apps --sort-by=.lastTimestamp | tail -15
  kubectl get events -n orkano-builds --sort-by=.lastTimestamp | tail -15
  kubectl describe pod lockdown-canary-apps -n orkano-apps 2>/dev/null
  kubectl describe pod lockdown-canary-build registry-tls-probe -n orkano-builds 2>/dev/null
  kubectl describe pod node-pull-probe -n "$INFRA_NS" 2>/dev/null
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
  local name=$1 deadline=$(( $(date +%s) + $2 )) ns=${3:-$BUILD_NS} c f
  while [ "$(date +%s)" -lt "$deadline" ]; do
    c=$(kubectl get job "$name" -n "$ns" -o 'jsonpath={.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null || true)
    f=$(kubectl get job "$name" -n "$ns" -o 'jsonpath={.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || true)
    [ "$c" = "True" ] && { echo complete; return; }
    [ "$f" = "True" ] && { echo failed; return; }
    sleep 5
  done
  echo timeout
}

# Poll a run-to-completion pod's both terminal phases (same reason as
# job_outcome): a wait-for-Succeeded blocks its full timeout on a Failed pod
# and surfaces a generic timeout instead of the real outcome.
pod_outcome() {
  local name=$1 ns=$2 deadline=$(( $(date +%s) + $3 )) p
  while [ "$(date +%s)" -lt "$deadline" ]; do
    p=$(kubectl get pod "$name" -n "$ns" -o 'jsonpath={.status.phase}' 2>/dev/null || true)
    if [ "$p" = "Succeeded" ] || [ "$p" = "Failed" ]; then echo "$p"; return; fi
    sleep 3
  done
  echo timeout
}

# Shared by probes 5 and 8: the identical TLS probe pod must pass both before
# the lockdown applies (acceptance of the registry itself) and after it (the
# lockdown's allow leg). $1 names the failure context.
run_tls_probe() {
  local outcome
  kubectl delete pod registry-tls-probe -n orkano-builds --ignore-not-found --grace-period=1
  kubectl apply -f "$DIR/07-registry-tls-probe.yaml"
  outcome="$(pod_outcome registry-tls-probe orkano-builds 240)"
  [ "$outcome" = "Succeeded" ] \
    || fatal "registry-tls-probe outcome=$outcome — $1"
  kubectl logs registry-tls-probe -n orkano-builds | grep -q 'OK: tls-verified push+pull' \
    || fatal "registry-tls-probe succeeded but the OK line is missing from its logs"
  kubectl logs registry-tls-probe -n orkano-builds | tail -3
  kubectl delete pod registry-tls-probe -n orkano-builds --grace-period=1
}

# Exec-based connect check from a long-lived canary pod (the probe-client
# pattern): exec rides the kubelet, not the pod network, so it works under
# any NetworkPolicy. $1 ns, $2 pod, $3 ip, $4 port.
canary_connect() {
  kubectl exec "$2" -n "$1" -- timeout 8 nc -z -w 5 "$3" "$4" >/dev/null 2>&1
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

# Reuse is only safe if every node carries both AppArmor mounts from
# kind-config.yaml (a cluster created without them can never enforce AppArmor)
# and the worker exists (probe 9 needs the cross-node topology).
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
    log "recreate kind cluster $CLUSTER (missing worker node or securityfs mount)"
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
# Probe 6 (the no-policy control) needs the lockdown absent; a previous run
# left it applied — including the rehearsed init-owned node allow.
kubectl delete -f "$REPO_ROOT/config/netpol/" --ignore-not-found
kubectl delete networkpolicy orkano-registry-ingress-nodes -n orkano-system --ignore-not-found
kubectl delete pod probe-server probe-client -n "$BUILD_NS" --ignore-not-found --grace-period=1
kubectl delete pod operator-canary system-canary -n orkano-system --ignore-not-found --grace-period=1
kubectl delete job buildkit-smoke buildkit-smoke-denied -n "$BUILD_NS" --ignore-not-found
kubectl delete job template-smoke static-template-smoke -n orkano-builds --ignore-not-found
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
kubectl apply -f "$REPO_ROOT/config/buildkit/"
# The Job template names the orkano-build ServiceAccount; without it the
# Job controller can never create probe 10's pod.
kubectl apply -f "$REPO_ROOT/config/rbac/serviceaccounts.yaml"
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
run_tls_probe "TLS push/pull acceptance failed"
echo "OK: product registry serves TLS from the internal CA; projected bundle verified both ways"

log "probe 6: lockdown controls — both deny-leg paths work while no policy exists"
REGISTRY_IP="$(kubectl get svc orkano-registry -n orkano-system -o 'jsonpath={.spec.clusterIP}')"
[ -n "$REGISTRY_IP" ] || fatal "orkano-registry Service has no ClusterIP"
INFRA_REGISTRY_IP="$(kubectl get svc registry -n "$INFRA_NS" -o 'jsonpath={.spec.clusterIP}')"
[ -n "$INFRA_REGISTRY_IP" ] || fatal "smoke registry Service has no ClusterIP"
# Canaries are exec targets, so each later denial is observed on the same pod
# that just proved the path open. The orkano-apps one draws PSA warnings
# (warn=restricted there) — harmless: it simulates exactly the plain user pod
# that warn label exists for.
kubectl delete pod lockdown-canary-apps -n orkano-apps --ignore-not-found --grace-period=1
kubectl delete pod lockdown-canary-build -n orkano-builds --ignore-not-found --grace-period=1
kubectl run lockdown-canary-apps -n orkano-apps --image=busybox:1.37 --restart=Never --command -- sleep 3600
kubectl run lockdown-canary-build -n orkano-builds --image=busybox:1.37 --restart=Never --command -- sleep 3600
kubectl wait --for=condition=Ready pod/lockdown-canary-apps -n orkano-apps --timeout=120s
kubectl wait --for=condition=Ready pod/lockdown-canary-build -n orkano-builds --timeout=120s
ctl_apps="" ctl_build=""
for _ in 1 2 3 4 5 6; do
  [ -z "$ctl_apps" ] && canary_connect orkano-apps lockdown-canary-apps "$REGISTRY_IP" 443 && ctl_apps=yes
  [ -z "$ctl_build" ] && canary_connect orkano-builds lockdown-canary-build "$INFRA_REGISTRY_IP" 5000 && ctl_build=yes
  [ -n "$ctl_apps" ] && [ -n "$ctl_build" ] && break
  sleep 5
done
[ -n "$ctl_apps" ] || fatal "control connect orkano-apps→product registry failed with no lockdown applied — later denials would prove nothing"
[ -n "$ctl_build" ] || fatal "control connect orkano-builds→policy-free smoke registry failed with no lockdown applied — later denials would prove nothing"
echo "OK: pre-lockdown controls"

log "apply the M1.2 lockdown manifests (config/netpol/)"
kubectl apply -f "$REPO_ROOT/config/netpol/"

log "probe 7: lockdown deny legs — each isolates one policy (doubles as the CNI-propagation barrier)"
# The apps canary targets the product registry: orkano-apps has no egress
# policies, so only the registry ingress policy can block it. The unlabeled
# builds canary targets the policy-free smoke registry: nothing guards that
# ingress, so only orkano-builds' default-deny egress can block it — probing
# both against the product registry would let a broken default-deny hide
# behind the registry ingress rule. Waiting until both legs observably deny
# is also the propagation barrier: probe 8's allow leg would trivially pass
# against rules not yet in the kernel.
denied_apps="" denied_build=""
for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
  [ -z "$denied_apps" ] && ! canary_connect orkano-apps lockdown-canary-apps "$REGISTRY_IP" 443 && denied_apps=yes
  [ -z "$denied_build" ] && ! canary_connect orkano-builds lockdown-canary-build "$INFRA_REGISTRY_IP" 5000 && denied_build=yes
  [ -n "$denied_apps" ] && [ -n "$denied_build" ] && break
  sleep 5
done
[ -n "$denied_apps" ] || fatal "orkano-apps still reaches the registry — the registry ingress lockdown is not enforced"
[ -n "$denied_build" ] || fatal "an unlabeled orkano-builds pod still has egress to a policy-free target — default-deny is not enforced"
kubectl delete pod lockdown-canary-apps -n orkano-apps --grace-period=1
kubectl delete pod lockdown-canary-build -n orkano-builds --grace-period=1
echo "OK: lockdown denies both directions, one policy per leg"

log "probe 8: lockdown allow leg — the build-labeled pod still TLS-pushes + pulls"
run_tls_probe "the lockdown's egress allowlist or registry ingress rule breaks real builds"
echo "OK: build-labeled pod keeps DNS + registry egress under the lockdown"

log "probe 8b: operator digest-resolution leg — allowed by label, not by namespace"
# The operator HEADs the registry through the Service (443 → DNAT 5000) to
# pin digests; its allow is a label contract until M1.5 deploys the real
# pod. The unlabeled control isolates the leg the same way probe 7 does:
# without it, a policy accidentally admitting all of orkano-system would
# pass. No propagation wait is owed — probe 7 already barriered this policy.
kubectl apply -f "$DIR/10-operator-canary.yaml"
kubectl wait --for=condition=Ready pod/operator-canary pod/system-canary -n orkano-system --timeout=120s
op_ok=""
for _ in 1 2 3 4 5 6; do
  if canary_connect orkano-system operator-canary "$REGISTRY_IP" 443; then op_ok=yes; break; fi
  sleep 5
done
[ -n "$op_ok" ] || fatal "operator-labeled canary cannot reach the registry — digest resolution would fail under the lockdown"
sys_denied=""
for _ in 1 2 3 4 5 6; do
  if ! canary_connect orkano-system system-canary "$REGISTRY_IP" 443; then sys_denied=yes; break; fi
  sleep 5
done
[ -n "$sys_denied" ] || fatal "an unlabeled orkano-system pod reaches the registry — the operator allow leaks beyond its label"
kubectl delete pod operator-canary system-canary -n orkano-system --grace-period=1
echo "OK: registry ingress admits the operator label and nothing else in orkano-system"

log "probe 9: node-originated kubelet-pull stand-in (cross-node)"
# Leg 1, soft: without a node allow, kindnet blocks cross-node host traffic
# (found empirically — this is why init must render the node allow at all).
# Logged, not asserted: a CNI that exempts host traffic makes the allow
# redundant but breaks nothing, and apps pods cannot reach host netns at PSA
# baseline, so the exemption is not a hole.
run_node_probe() {
  kubectl delete pod node-pull-probe -n "$INFRA_NS" --ignore-not-found --grace-period=1 >/dev/null
  sed -e "s|__REGISTRY_IP__|$REGISTRY_IP|" -e "s|__NODE__|$CLUSTER-control-plane|" \
    "$DIR/08-node-pull-probe.yaml" | kubectl apply -f - >/dev/null
  pod_outcome node-pull-probe "$INFRA_NS" 120
}
if [ "$(run_node_probe)" = "Succeeded" ]; then
  echo "substrate fact: this CNI exempts cross-node host traffic from pod selectors (node allow is redundant here)"
else
  echo "substrate fact: this CNI subjects cross-node host traffic to the ingress policy (node allow is load-bearing)"
fi

log "rehearse init's node-pull allow (one /32 per node InternalIP — init owns this at install, M1.5)"
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
} | kubectl apply -f -
outcome="$(run_node_probe)"
[ "$outcome" = "Succeeded" ] \
  || fatal "node-pull-probe outcome=$outcome — still blocked despite the per-node allow; kubelet pulls would break, the init contract does not hold on this CNI"
kubectl delete pod node-pull-probe -n "$INFRA_NS" --grace-period=1
# Kubelet's readiness/liveness probes are host-originated too; by now the
# policy has been live for minutes (probes fire every 10s, three failures
# flip readiness), so Available still holding is part of claim 5.
kubectl wait --for=condition=Available deploy/orkano-registry -n orkano-system --timeout=60s >/dev/null \
  || fatal "registry went unready under the ingress policy — kubelet probe traffic is being blocked"
echo "OK: node-originated pull path and kubelet probes survive the lockdown"

log "probe 10: product build Job template — git-context build + TLS push under the full lockdown"
# The golden file is the Go template's exact output (pinned by the unit test
# in operator/internal/buildjob); applying it here is the end-to-end half of
# the Job-template acceptance: baseline PSA admits it for real, the AppArmor
# profile confines it, the lockdown allows exactly its traffic, and the push
# rides TLS through the projected CA via buildkitd.toml.
apply_job "$DIR/09-build-job-template.yaml"
outcome="$(job_outcome template-smoke 600 orkano-builds)"
[ "$outcome" = "complete" ] \
  || fatal "template build $outcome (expected complete) — the rendered product Job cannot build+push under the lockdown"
kubectl logs job/template-smoke -n orkano-builds --tail=5
echo "OK: the rendered product build Job builds a public repo and TLS-pushes to the registry"

log "probe 11: static build Job — generated Dockerfile injected (dockerfilekey), COPY from git context, TLS push"
# The static golden is Render's exact output for a Static build: an init
# container writes the COPY-only Dockerfile and buildkit reads it via
# dockerfilekey + --local while the git URL stays the COPY context (the repo
# has no Dockerfile). This is the static-strategy acceptance and the in-cluster
# confirmation of the undocumented dockerfilekey opt on the pinned rootless
# v0.30.0 image. Plain kubectl apply, NOT apply_job: a single shared
# SMOKE_GIT_CONTEXT can't serve both probe 10 (needs a Dockerfile in the
# context) and probe 11 (needs a public/ dir), and here the git URL is the COPY
# source itself — so the static-fixture context is deliberately pinned.
kubectl apply -f "$DIR/11-static-build-job-template.yaml"
outcome="$(job_outcome static-template-smoke 600 orkano-builds)"
[ "$outcome" = "complete" ] \
  || fatal "static build $outcome (expected complete) — the generated-Dockerfile injection (dockerfilekey) failed under the lockdown"
kubectl logs job/static-template-smoke -n orkano-builds --tail=5
echo "OK: the static build Job injects a generated Dockerfile and TLS-pushes the static image"

log "PASS — substrate facts"
echo "cluster: $CLUSTER ($(kind version))"
echo "node image: $(kubectl get nodes -o 'jsonpath={.items[0].status.nodeInfo.kubeletVersion} {.items[0].status.nodeInfo.containerRuntimeVersion} {.items[0].status.nodeInfo.kernelVersion}')"
echo "cni pods: $(kubectl get pods -n kube-system -o name | grep -E 'kindnet|calico|cilium|cni' | tr '\n' ' ')"
