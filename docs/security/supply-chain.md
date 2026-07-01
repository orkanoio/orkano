# Supply-chain policy

Orkano's installer and images are an attack vector for every downstream user (PLANNING constraint), so this hygiene exists before any real user does. This document is where thresholds live — configs reference it; nothing here is tribal knowledge.

## Scanning thresholds

- **govulncheck** (every PR and push, `make vulncheck`): **any finding fails CI.** govulncheck reports only *reachable* vulnerabilities, so zero tolerance is realistic. There is no suppression file; introducing one requires a PR with per-entry justification and an expiry date.
- **Trivy** (release images, in the release workflow): `--severity CRITICAL,HIGH --ignore-unfixed --exit-code 1`. Any `.trivyignore` entry requires an inline justification comment and an expiry date.
- A weekly scheduled scan of already-published images is deferred to Phase 5 (deliberately: one maintainer, one less cron to babysit until there are users).

## Release integrity

Every release from v0.0.1 ships:

- **cosign keyless signatures** on images and the checksums file (Sigstore, CI OIDC identity — no long-lived signing keys to steal, consistent with INV-07's spirit).
- **syft SBOMs** (SPDX) per archive and per image.
- **SLSA provenance** at **Build L2** via GitHub native artifact attestations (`actions/attest-build-provenance`). The L3 upgrade (slsa-github-generator's isolated builder, or its successor) is a Phase 5 launch-hardening task — L2 now is one first-party step; L3 today is multi-job choreography with a history of breaking releases, the maintenance a solo project cannot pay.

Verification commands users can run (exercised for real against v0.0.1):

```sh
cosign verify \
  --certificate-identity-regexp 'https://github.com/orkanoio/orkano' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/orkanoio/orkano-operator:<version>

cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/orkanoio/orkano' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

gh attestation verify checksums.txt --owner orkanoio
gh attestation verify oci://ghcr.io/orkanoio/orkano-operator:<version> --owner orkanoio
```

Note: goreleaser image tags carry the version without the `v` prefix.

## Rules that keep the pipeline honest

- **No `RUN` steps in product Dockerfiles.** Images are `COPY` of a prebuilt binary onto a digest-pinned distroless base (ADR-0007). This is what lets CI cross-compile linux/arm64 without qemu; a PR adding a `RUN` step must justify adding emulation.
- **Workspace off at release:** goreleaser builds run with `GOWORK=off` so published binaries resolve third-party dependencies from `go.mod`/`go.sum` alone; the first-party `api` module comes from the same tagged source tree via a directory replace (ADR-0009).
- **Actions pinned:** authored at major tags; Renovate (`config:best-practices`) rewrites them to SHA-pins-with-comment once installed.

## Renovate (not Dependabot)

Renovate because, for one reviewer: PR grouping and weekly scheduling cap review load (Dependabot's grouping is far cruder); it digest-pins GitHub Actions and Dockerfile bases with readable comments, which Dependabot cannot; custom managers can later track Makefile-pinned tool versions; the hosted Mend app is free and self-hosting it remains an exit hatch (self-hosted-first principle).

## Required checks for branch protection (apply on GitHub after first push)

`lint (.)` · `lint (api)` · `test` · `build (amd64)` · `build (arm64)` · `vulncheck` · `web`

`web` guards the release's UI path: it builds the SPA and runs the webdist-tagged embed test, the check that stops a broken Vite build or misconfigured outDir from shipping a UI-less dashboard binary.

## Throwaway-release runbook (deferred until the repo is on GitHub)

Tag `v0.0.1` → release workflow runs goreleaser (build, archives, multi-arch images, SBOMs) → Trivy gates → cosign signs keylessly → provenance attested → verify both commands above from a clean machine. Note: keyless signing logs to Rekor permanently — the artifacts of a throwaway tag can be deleted, the transparency-log entry cannot. Acceptable.
