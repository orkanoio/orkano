# ADR-0007: Distroless static base images, non-root, read-only rootfs

- Status: Accepted
- Date: 2026-06-11

## Context

Every product image Orkano publishes runs inside user clusters, so its base is part of the attack surface we ship to other people. All server-side binaries are pure-Go and static (`CGO_ENABLED=0`), which makes the base image's only jobs: CA certificates, tzdata, a non-root user, and nothing else.

## Decision

Every product image is built `FROM gcr.io/distroless/static-debian12:nonroot`, **pinned by digest** (Renovate keeps the pin fresh), runs as UID 65532 with `readOnlyRootFilesystem: true` in its manifests, and contains no shell, package manager, or libc. Dockerfiles contain `COPY` of the prebuilt binary and nothing executable — no `RUN` steps, per the supply-chain policy.

Debugging story: there is deliberately nothing to shell into; `kubectl debug` with an ephemeral container is the documented path.

## Consequences

- Runtime CVE surface approaches the binary itself; Trivy findings against the base are rare and meaningful.
- No `RUN` steps keeps CI free of qemu for arm64 image assembly (the build matrix cross-compiles).
- Contributors cannot `docker exec` a shell into product images; one line in CONTRIBUTING-adjacent docs when this first bites.

## Alternatives considered

- **Chainguard static** — excellent images, but the free tier now publishes only `:latest`/`:latest-dev` (versioned tags moved to the paid catalog, verified 2026-06-11). Digest-pinning a latest-only stream means uncontrolled version jumps on every Renovate run, and depending on a vendor's paid tier for reproducible builds violates the open-source-friendly principle.
- **alpine** — a shell and a package manager we'd ship solely so attackers can use them.
- **scratch** — saves nothing over distroless static but loses CA certs, tzdata, and the nonroot passwd entry, each of which we'd reinvent.
- **distroless `base-debian12`** — carries libc the static binaries don't need.
