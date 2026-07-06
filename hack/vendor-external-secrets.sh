#!/usr/bin/env bash
# Vendors the External Secrets Operator manifest for `orkano init
# --secrets-vault` (ADR-0018): downloads the pinned helm chart, verifies its
# sha256, renders it with the ADR-0018 scoping values, applies the three
# documented local patches, and writes config/external-secrets/
# external-secrets.yaml. Run from the repo root. Requires helm (v3), curl,
# shasum, python3.
#
# Bumping ESO: update the four pins below (re-resolve IMAGE_DIGEST as a
# multi-arch INDEX digest covering amd64+arm64), re-run, re-inspect the diff —
# every patch below is anchored to exact upstream bytes and fails loudly if
# the chart changed shape, which is the cue to re-review the render.
set -euo pipefail

CHART_VERSION="2.7.0"
CHART_SHA256="a2ce165cc59b74c88451c9e21eb72c329b392506c53763df19061e62ec29ac60"
APP_VERSION="v2.7.0"
IMAGE="ghcr.io/external-secrets/external-secrets"
# Multi-arch OCI index digest for ${IMAGE}:${APP_VERSION} (amd64+arm64).
IMAGE_DIGEST="sha256:6615aaea8ff44924d9d7dbc99982a130c82913f7583e212fa3aeebc6dc21fbf9"

CHART_URL="https://github.com/external-secrets/external-secrets/releases/download/helm-chart-${CHART_VERSION}/external-secrets-${CHART_VERSION}.tgz"
OUT="config/external-secrets/external-secrets.yaml"

for tool in helm curl shasum python3; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }
done
[ -f go.mod ] || { echo "run from the repo root" >&2; exit 1; }

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

echo "downloading chart ${CHART_VERSION}..."
curl -sSfL --max-time 120 -o "$workdir/chart.tgz" "$CHART_URL"
echo "${CHART_SHA256}  $workdir/chart.tgz" | shasum -a 256 -c - >/dev/null \
  || { echo "chart sha256 mismatch" >&2; exit 1; }

# The ADR-0018 render values. Everything not listed keeps the chart default.
cat > "$workdir/values.yaml" <<'VALUES'
# Confine ESO to orkano-apps: controller RBAC becomes a Role in orkano-apps,
# --namespace=orkano-apps scopes the manager cache, and the cluster-scoped
# reconcilers are disabled.
scopedRBAC: true
scopedNamespace: orkano-apps

rbac:
  # No blanket serviceaccounts/token create; Orkano's flows use static
  # credential Secrets, never SA-token store auth (ADR-0018).
  serviceAccountTokenCreate: false
  # Nothing in Orkano consumes servicebinding.io; drop that ClusterRole.
  servicebindings:
    create: false
  # Aggregation only works on ClusterRoles; under scopedRBAC the view/edit
  # objects are namespaced Roles, so the aggregate labels would be inert.
  aggregateToView: false
  aggregateToEdit: false

# The cluster-scoped kinds and PushSecret are outside the v1 surface
# (ADR-0018 decisions 3 + 6): don't install CRDs nobody reconciles.
processClusterStore: false
processClusterExternalSecret: false
processClusterPushSecret: false
processPushSecret: false
crds:
  createClusterSecretStore: false
  createClusterExternalSecret: false
  createClusterPushSecret: false
  createPushSecret: false

# Explicit requests/limits (Orkano convention: memory limit, no CPU limit).
resources:
  requests:
    cpu: 50m
    memory: 128Mi
  limits:
    memory: 512Mi
webhook:
  resources:
    requests:
      cpu: 20m
      memory: 64Mi
    limits:
      memory: 128Mi
certController:
  resources:
    requests:
      cpu: 20m
      memory: 64Mi
    limits:
      memory: 256Mi
VALUES

echo "rendering..."
helm template external-secrets "$workdir/chart.tgz" \
  --namespace external-secrets \
  --values "$workdir/values.yaml" \
  --include-crds > "$workdir/rendered.yaml"

echo "patching..."
CHART_VERSION="$CHART_VERSION" CHART_SHA256="$CHART_SHA256" \
APP_VERSION="$APP_VERSION" IMAGE="$IMAGE" IMAGE_DIGEST="$IMAGE_DIGEST" \
CHART_URL="$CHART_URL" \
python3 - "$workdir/rendered.yaml" "$OUT" <<'PYEOF'
import os
import sys

rendered_path, out_path = sys.argv[1], sys.argv[2]
env = os.environ
rendered = open(rendered_path).read()


def replace_exactly(haystack, old, new, want, what):
    found = haystack.count(old)
    if found != want:
        sys.exit(f"anchor {what!r}: expected {want} occurrence(s), found {found} "
                 "- upstream chart changed shape, re-review the render")
    return haystack.replace(old, new)


# Patch 2: digest-pin the one image (three deployments, one repo).
tag_ref = f"image: {env['IMAGE']}:{env['APP_VERSION']}\n"
pinned_ref = f"image: {env['IMAGE']}:{env['APP_VERSION']}@{env['IMAGE_DIGEST']}\n"
rendered = replace_exactly(rendered, tag_ref, pinned_ref, 3, "image ref")

# Patch 3: drop the cert-controller ClusterRole's cluster-wide secrets
# get/list/watch + pinned secrets update/patch + unused leases rules (the
# contiguous tail of that ClusterRole); a namespaced, name-pinned Role
# replaces the secrets access below. The controller only ever GETs/UPDATEs
# its one webhook TLS Secret - Secret caching is disabled in ESO's
# cmd/controller/certcontroller.go, so no list/watch ever happens.
cluster_secret_rules = '''  - apiGroups:
    - ""
    resources:
    - "secrets"
    verbs:
    - "get"
    - "list"
    - "watch"
  - apiGroups:
    - ""
    resources:
    - "secrets"
    resourceNames:
    - "external-secrets-webhook"
    verbs:
    - "update"
    - "patch"
  - apiGroups:
    - "coordination.k8s.io"
    resources:
    - "leases"
    verbs:
    - "get"
    - "create"
    - "update"
    - "patch"
---
# Source: external-secrets/templates/cert-controller-rbac.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
'''
scoped_replacement = '''---
# Orkano local patch (ADR-0018): the cert-controller's Secret access,
# namespaced and name-pinned instead of upstream's cluster-wide
# secrets get/list/watch ClusterRole rule. Its unused leases rule
# (leader election is off in this render) is dropped with it.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: external-secrets-cert-controller-webhook-secret
  namespace: external-secrets
  labels:
    app.kubernetes.io/name: external-secrets-cert-controller
    app.kubernetes.io/instance: external-secrets
rules:
  - apiGroups:
    - ""
    resources:
    - "secrets"
    resourceNames:
    - "external-secrets-webhook"
    verbs:
    - "get"
    - "update"
    - "patch"
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: external-secrets-cert-controller-webhook-secret
  namespace: external-secrets
  labels:
    app.kubernetes.io/name: external-secrets-cert-controller
    app.kubernetes.io/instance: external-secrets
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: external-secrets-cert-controller-webhook-secret
subjects:
  - kind: ServiceAccount
    name: external-secrets-cert-controller
    namespace: external-secrets
---
# Source: external-secrets/templates/cert-controller-rbac.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
'''
rendered = replace_exactly(
    rendered, cluster_secret_rules, scoped_replacement, 1,
    "cert-controller secrets+leases rules")

# Belt-and-braces: after the patch, no ClusterRole in the file may mention
# secrets at all.
in_cluster_role = False
for doc in rendered.split("\n---\n"):
    if "\nkind: ClusterRole\n" in "\n" + doc:
        if '"secrets"' in doc:
            sys.exit("a ClusterRole still grants secrets - patch failed")

header = f'''# External Secrets Operator {env['APP_VERSION']} - vendored for
# `orkano init --secrets-vault` (ADR-0018). NOT part of the base install:
# internal/install writes it to the k3s auto-deploy dir only on opt-in.
#
# Source: helm chart external-secrets-{env['CHART_VERSION']}.tgz
#   {env['CHART_URL']}
#   sha256: {env['CHART_SHA256']}
# Generated by hack/vendor-external-secrets.sh - do not edit by hand; re-run
# the script to reproduce this file byte-for-byte, edit its pins to bump.
#
# This is NOT upstream's standalone external-secrets.yaml. It is `helm
# template` with the ADR-0018 scoping values (see the script):
#   - scopedRBAC=true + scopedNamespace=orkano-apps: the controller's RBAC is
#     a namespaced Role in orkano-apps, --namespace=orkano-apps scopes its
#     cache, and the cluster-scoped reconcilers are disabled. ESO can touch
#     Secrets in orkano-apps and nowhere else.
#   - rbac.serviceAccountTokenCreate=false: no blanket SA token-create grant.
#   - ClusterSecretStore / ClusterExternalSecret / (Cluster)PushSecret CRDs
#     and reconcilers are not installed (ADR-0018 decisions 3 + 6).
#   - servicebindings ClusterRole + aggregate-to-view/edit labels off;
#     explicit resource requests/limits on all three deployments.
# Plus three local patches applied by the script on top of that render:
#   1. This restricted-PSA-labeled external-secrets namespace object.
#   2. The single image is digest-pinned to its multi-arch (amd64+arm64)
#      OCI index: {env['IMAGE_DIGEST']}.
#   3. The cert-controller's Secret access moves off its ClusterRole into a
#      Role in external-secrets, resourceNames-pinned to the webhook TLS
#      Secret; its unused leases rule is dropped. The only ClusterRole left
#      in the file holds zero Secret access.
#
# Upstream is Apache-2.0: Copyright the External Secrets Operator authors.
apiVersion: v1
kind: Namespace
metadata:
  name: external-secrets
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/warn: restricted
    pod-security.kubernetes.io/audit: restricted
'''

with open(out_path, "w") as f:
    f.write(header + rendered)
print(f"wrote {out_path}")
PYEOF

echo "done: $OUT"
