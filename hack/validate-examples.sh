#!/usr/bin/env bash
# Proves the Phase 0 exit criterion in a real cluster: every example YAML is
# accepted by the generated CRDs, every negative fixture is rejected, and the
# CEL transition rules reject mutation.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=orkano-dev
CTX=kind-$CLUSTER
KC() { kubectl --context "$CTX" "$@"; }

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER"

KC apply --server-side -f config/crd/ >/dev/null
# Examples 07/08 are ESO kinds (ADR-0018). Install just the two CRDs they use,
# extracted from the vendored render by their helm Source headers — applying
# the whole file would deploy the operator itself onto the dev cluster.
awk '/^# Source: external-secrets\/templates\/crds\/(secretstore|externalsecret)\.yaml$/{p=1; print "---"} /^---$/{p=0} p' \
  config/external-secrets/external-secrets.yaml | KC apply --server-side -f - >/dev/null
KC wait --for=condition=Established crd/apps.orkano.io crd/builds.orkano.io crd/domains.orkano.io crd/postgreses.orkano.io crd/secretstores.external-secrets.io crd/externalsecrets.external-secrets.io --timeout=60s >/dev/null
KC create namespace orkano-apps --dry-run=client -o yaml | KC apply -f - >/dev/null

for f in docs/examples/*.yaml; do
  KC apply --dry-run=server -f "$f" >/dev/null
  echo "PASS accepted: $f"
done

for f in hack/testdata/invalid/*.yaml; do
  if KC apply --dry-run=server -f "$f" >/dev/null 2>&1; then
    echo "FAIL should have been rejected: $f"
    exit 1
  fi
  echo "PASS rejected: $f"
done

# Transition rules compare against oldSelf, which a server-side dry-run of a
# create can never exercise — so create for real, mutate, expect rejection.
KC apply -f hack/testdata/transition-base.yaml >/dev/null
trap 'KC delete -f hack/testdata/transition-base.yaml --ignore-not-found >/dev/null' EXIT
if KC patch build immutable-probe -n orkano-apps --type merge -p '{"spec":{"timeoutSeconds":901}}' >/dev/null 2>&1; then
  echo "FAIL build spec mutation should have been rejected"
  exit 1
fi
echo "PASS rejected: build spec mutation"
if KC patch domain immutable-probe -n orkano-apps --type merge -p '{"spec":{"host":"other.example.com"}}' >/dev/null 2>&1; then
  echo "FAIL domain host mutation should have been rejected"
  exit 1
fi
echo "PASS rejected: domain host mutation"
if KC patch postgres immutable-probe -n orkano-apps --type merge -p '{"spec":{"version":"17"}}' >/dev/null 2>&1; then
  echo "FAIL postgres version mutation should have been rejected"
  exit 1
fi
echo "PASS rejected: postgres version mutation"

echo "OK: all examples accepted, all invalid fixtures and mutations rejected"
