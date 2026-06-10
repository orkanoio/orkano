# Contributing to Orkano

Thanks for considering a contribution. Orkano is maintained by one person in their spare time (~10–15 hrs/week); everything below is shaped by that reality.

## Dev setup

```sh
git clone https://github.com/orkanoio/orkano && cd orkano
make all   # lint + test + build — must be green before you start
```

The pinned Go toolchain downloads automatically; golangci-lint installs into `./bin`. Alternatively, open the repo in the provided [devcontainer](.devcontainer/devcontainer.json) — it runs `make all` on create.

## DCO sign-off (required)

Every commit must carry a `Signed-off-by` line certifying the [Developer Certificate of Origin](https://developercertificate.org/):

```sh
git commit -s
```

PRs with unsigned commits cannot be merged. No CLA — see [ADR-0002](docs/adr/0002-license-agpl-3-0-only-and-dco.md).

## Pull requests

- One TASKS.md item or issue per PR; small PRs get reviewed, large ones wait.
- Reference the task or issue in the description, and note any implementation calls you made.
- Prefer integration tests over heavily mocked unit tests, especially around the Kubernetes API.
- Match the existing code style: idiomatic Go, `gofmt`, errors wrapped with `%w`, contexts threaded through, comments only where the *why* is non-obvious.
- Anything touching architecture, public API, security posture, or scope needs an ADR — open an issue to discuss before writing code.

## Where to start

Doctor checks are the designated good-first-issue surface: each check is a small, self-contained probe with a clear contract (`api/check`). Look for `good-first-issue` labels.

## Before you file an issue

1. Check [PRD.md](PRD.md) Non-Goals — feature requests outside v1 scope go to the TASKS.md backlog, not the issue tracker.
2. Search existing issues, including closed ones.
3. For bugs: include the Orkano commit/version, platform (k3s or BYO, amd64/arm64), and what you expected.
4. For security issues: **never open a public issue** — see [SECURITY.md](SECURITY.md).

## Labels

`kind/{bug,feature,docs,security,question}` · `area/{api,operator,receiver,dashboard,cli,build,ci,security}` · `triage/{needs-triage,accepted}` · `good-first-issue` · `help-wanted` · `blocked`

## Response cadence

Triage happens roughly weekly, not 24/7. A slow response means the maintainer is busy, not that your contribution is unwelcome.
