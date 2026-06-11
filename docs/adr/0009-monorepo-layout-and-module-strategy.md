# ADR-0009: Monorepo layout with a separate api module

- Status: Accepted
- Date: 2026-06-10

## Context

The Phase 0 plan prescribes a monorepo (`/api`, `/operator`, `/cli`, `/receiver`, `/dashboard`, `/docs`, `/hack`) with `/api` importable by third parties. Importability is a dependency-graph question as much as a license question: anyone importing the CRD types or the doctor check contract must not inherit controller-runtime, chi, or the rest of the product's dependency tree through Go's minimal version selection.

## Decision

Two Go modules, tied together by a committed `go.work`:

- `github.com/orkanoio/orkano/api` — CRD types (`api/v1alpha1`) and the doctor check contract (`api/check`). Its only permitted heavyweight dependency is `k8s.io/apimachinery`; anything heavier is rejected in review.
- `github.com/orkanoio/orkano` (root) — operator, cli, receiver, and later the dashboard API. Created now with version-printing stub mains so the CI build matrix and the release pipeline have real artifacts to prove themselves on from v0.0.1.

Directories materialize when their first real content lands; their contracts are stated in the root README. Spike code under `hack/spikes/` gets throwaway modules outside the workspace. Release builds run with `GOWORK=off` so published binaries resolve dependencies from `go.mod` alone.

## Consequences

- Third parties can depend on `api` without pulling product dependencies; per ADR-0002 they must still be AGPL-compatible.
- Two `go.mod` files to maintain and tag; accepted as the minimum that satisfies the importability contract (module-per-directory was the alternative that doesn't).
- The committed `go.work` makes fresh-clone `make all` work with zero setup, at the cost of the `GOWORK=off` discipline at release time, recorded here for the release pipeline.
- The root module requires `api` through a directory replace (`replace … => ./api`), so `GOWORK=off` release builds compile `api` from the release checkout rather than a sum-verified module version, and `go install github.com/orkanoio/orkano/cli@<version>` is not supported — the CLI ships via release archives. The alternative (tagging `api/vX.Y.Z` in lockstep and dropping the replace) can be revisited if `go install` support ever matters.

## Alternatives considered

- **Single root module** — importers inherit the full product dependency graph; rejected.
- **Module per directory** — five modules to version and tag for one maintainer; rejected.
- **Defer the root module to Phase 1** — leaves CI and the v0.0.1 release pipeline with nothing to build; rejected.
