# Spike 2 — controller-runtime scaffold: findings

Date: 2026-06-11. Machine: macOS arm64 (darwin/arm64), Go 1.26.4. Code in this directory is throwaway; this file is the deliverable.

## Version set

Exact working pair, taken from this spike's `go.mod`/`go.sum` after a green `make test`. This table seeds the real operator's `go.mod` in Phase 1.

| Component | Version |
|---|---|
| sigs.k8s.io/controller-runtime | v0.24.1 |
| k8s.io/api | v0.36.0 |
| k8s.io/apimachinery | v0.36.0 |
| k8s.io/client-go | v0.36.0 |
| sigs.k8s.io/controller-tools (controller-gen) | v0.21.0 |
| sigs.k8s.io/controller-runtime/tools/setup-envtest | v0.24.1 |
| envtest assets (kube-apiserver/etcd) | K8s 1.36.0, darwin-arm64 |
| Go | go1.26.4 |

Rule of thumb confirmed: controller-runtime v0.24.x ↔ k8s.io/* v0.36.x (K8s 1.36) ↔ controller-tools v0.21.x. setup-envtest is now semver-tagged in lockstep with controller-runtime (v0.24.1), so both tools pin cleanly via `tool` directives in `go.mod` (`go get -tool …`, run with `go tool controller-gen` / `go tool setup-envtest`).

## Dev loop

- envtest on darwin/arm64 works natively: **yes**. `setup-envtest use 1.36.0 -p path` downloaded native arm64 kube-apiserver+etcd binaries in seconds, no Rosetta, no flags.
- Asset cache lands in `~/Library/Application Support/io.kubebuilder.envtest/k8s/1.36.0-darwin-arm64`. The path contains a **space**, so `KUBEBUILDER_ASSETS` must be quoted everywhere (the Makefile does).
- Warm edit→test cycle (touch a controller source file, `make test`): **~9.0s wall** (cold first run incl. compile of deps: ~13.4s). Of the warm 9s, ~4s is envtest apiserver/etcd startup, ~2.2s the actual test. Comfortably under the 30s target.
- Surprise: this spike lives under the repo root, whose `go.work` deliberately does not include it. Every `go`/`go tool` invocation needs `GOWORK=off`, otherwise `./...` patterns fail with "directory prefix . does not contain modules listed in go.work". The Makefile exports it; remember this for any future nested throwaway module.

## Patterns proven

All observed in the passing envtest suite (`internal/controller/suite_test.go`, single lifecycle test, 3 subtests):

- **Finalizer add/remove**: `controllerutil.AddFinalizer`/`RemoveFinalizer`/`ContainsFinalizer` (`pkg/controller/controllerutil`). Added on first reconcile; the resulting update event triggers the follow-up reconcile that sets status. On delete, cleanup ran (log line observed with the spec message) and the finalizer removal let the object actually disappear (Get → NotFound).
- **Status conditions via `meta.SetStatusCondition`** (`k8s.io/apimachinery/pkg/api/meta`): Ready=True / reason MessageAccepted, with `condition.ObservedGeneration` honored and verified equal to `metadata.generation`. Returns a changed bool, used to skip no-op updates.
- **Status subresource updates**: `r.Status().Update(ctx, &app)` against the CRD's `+kubebuilder:subresource:status`; `status.observedGeneration` tracked alongside conditions.
- **ownerReferences**: not needed for this spike, not exercised.
- Conditions field needs `+listType=map` / `+listMapKey=type` markers for a server-side-apply-friendly CRD schema.

## Notes for Phase 1

- **Manager metrics options changed vs older docs**: `Options.MetricsBindAddress` (string) is long gone; it's `Metrics: metricsserver.Options{...}` from `pkg/metrics/server`. In current kubebuilder scaffolds the metrics endpoint is **disabled by default** (`--metrics-bind-address=0`) and, when enabled, served via HTTPS with optional authn/authz (`SecureServing`, `FilterProvider: filters.WithAuthenticationAndAuthorization`). The spike sets `BindAddress: "0"` in tests to avoid port clashes; do the same in the real operator's test suite.
- Pin controller-gen and setup-envtest as `tool` directives in the operator's go.mod (works on Go ≥1.24; clean on 1.26.4). No `hack/tools.go` pattern, no global installs, versions live in go.mod/go.sum.
- `client.IgnoreNotFound` on the initial Get, `logf.FromContext(ctx)` for per-reconcile structured logging (controller/namespace/name/reconcileID come for free) — both work as currently documented.
- `app.DeletionTimestamp.IsZero()` gates the delete path; remember reconcile still fires for objects with a deletion timestamp as long as finalizers remain.
- k8s.io/api ended up indirect here (the spike only needs apimachinery + client-go); the real operator will import it directly for Deployments/Services — keep it pinned to the same v0.36.x as the rest.
- Plain `testing` + `wait.PollUntilContextTimeout` was enough for envtest assertions; ginkgo/gomega is not a forced dependency of the pattern. Decide deliberately in Phase 1.
- Run the manager in a goroutine and wait on `mgr.GetCache().WaitForCacheSync(ctx)` before asserting, otherwise early Gets race the watch startup.
