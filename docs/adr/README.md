# Architecture Decision Records

Rules:
- Numbers are assigned at proposal time; gaps are expected (the Phase 0 plan pre-reserved 0001–0008).
- Statuses: `Proposed` · `Accepted` · `Superseded by ADR-NNNN`.
- Accepted ADRs are immutable — supersede, never edit.

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-license-agpl-3-0-only-and-dco.md) | License under AGPL-3.0-only with DCO sign-offs | Accepted |
| [0003](0003-bootstrap-auth.md) | Bootstrap auth with a one-time install token, forced TOTP, and opaque sessions | Accepted |
| [0004](0004-exposure-defaults.md) | Ship the dashboard unreachable from the internet by default | Accepted |
| [0005](0005-api-group-naming-scoping.md) | API group orkano.io with namespaced kinds | Accepted |
| [0006](0006-domain-cr-and-tls.md) | Model domains as a separate CR with always-on TLS | Accepted |
| [0007](0007-base-image-policy.md) | Distroless static base images, non-root, read-only rootfs | Accepted |
| [0008](0008-dashboard-stack.md) | Dashboard on React, TypeScript, Vite, TanStack Query, Tailwind, shadcn/ui | Accepted |
| [0009](0009-monorepo-layout-and-module-strategy.md) | Monorepo layout with a separate api module | Accepted |
| [0010](0010-cel-only-validation-no-webhook.md) | Validate with OpenAPI and CEL only — no admission webhook in v1alpha1 | Accepted |
| [0011](0011-api-versioning-deprecation.md) | API versioning and deprecation policy | Accepted |
