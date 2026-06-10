#!/usr/bin/env bash
# Spike 1 — distilled WORKING path (see FINDINGS.md for the full attempt ladder).
# Prereqs: Lima VM "orkano-spike" running k3s, kubectl pointed at it:
#   export KUBECONFIG=$HOME/.lima/orkano-spike/copied-from-guest/kubeconfig.yaml
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NS=orkano-build-spike
INFRA=orkano-spike-infra

# --- 1. Namespaces (build ns starts at PSA restricted; the proven minimum is baseline)
kubectl apply -f "$DIR/00-namespaces.yaml"

# --- 2. Registry + netpol probe pods
kubectl apply -f "$DIR/01-registry.yaml" -f "$DIR/02-netpol-probe.yaml"
kubectl wait --for=condition=Ready pod/probe-server pod/probe-client -n "$NS" --timeout=300s
kubectl wait --for=condition=Available deploy/registry -n "$INFRA" --timeout=300s

# --- 3. NetworkPolicy enforcement probe
# Expected: SUCCEEDS (nginx HTML) — no policies yet.
kubectl exec probe-client -n "$NS" -- \
  wget -qO- -T 5 http://probe-server.$NS.svc.cluster.local:8080 >/dev/null
echo "OK: baseline connectivity"

kubectl apply -f "$DIR/03-deny-all.yaml"
sleep 3
# Expected: FAILS (kube-router REJECT; DNS egress is blocked too) — proves enforcement.
if kubectl exec probe-client -n "$NS" -- \
  wget -qO- -T 5 http://probe-server.$NS.svc.cluster.local:8080 >/dev/null 2>&1; then
  echo "FATAL: deny-all not enforced — is k3s running with --disable-network-policy?" >&2
  exit 1
fi
echo "OK: deny-all enforced"

# --- 4. Egress allowlist (DNS + registry + tcp/443) and build context
kubectl apply -f "$DIR/04-egress-allowlist.yaml" -f "$DIR/05-build-context.yaml"

# --- 5. Node prerequisite: AppArmor profile with userns + mount.
# The cri default profile silently denies mount(2) (no audit entry!) and kills
# the rootlesskit child with "failed to share mount point: /".
limactl shell orkano-spike -- sudo bash -c \
  "cp /dev/stdin /etc/apparmor.d/orkano-buildkit && apparmor_parser -r /etc/apparmor.d/orkano-buildkit" \
  < "$DIR/apparmor-orkano-buildkit.profile"
echo "OK: apparmor profile loaded"

# --- 6. PSA: the empirically minimal admittable level for the working job is BASELINE.
# (restricted blocks rootlesskit's clone(CLONE_NEWUSER) via RuntimeDefault seccomp
# and requires allowPrivilegeEscalation:false, which breaks the file-caps newuidmap.)
kubectl label ns "$NS" \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/warn=baseline \
  pod-security.kubernetes.io/audit=baseline --overwrite

# --- 7. The minimal working build job (attempt F2). Expected: Complete in well under a minute.
kubectl delete job buildkit-f2 -n "$NS" --ignore-not-found
kubectl apply -f "$DIR/job-f2-localhost-apparmor.yaml"
kubectl wait --for=condition=complete job/buildkit-f2 -n "$NS" --timeout=600s
kubectl logs job/buildkit-f2 -n "$NS" --tail=10

# --- 8. Verify the pushed image from the policy-free infra namespace.
# Expected: {"name":"spike","tags":[... "f2" ...]}
kubectl run reg-check -n "$INFRA" --image=busybox:1.37 --restart=Never --rm -i --quiet -- \
  wget -qO- -T 5 http://registry.$INFRA.svc.cluster.local:5000/v2/spike/tags/list
echo
echo "SPIKE PASSED: rootless BuildKit built and pushed under PSA baseline."
echo "Teardown: limactl delete orkano-spike"
