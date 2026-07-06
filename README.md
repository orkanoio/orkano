# Orkano

Open-source, self-hosted PaaS that makes Kubernetes as easy as Heroku — built for true high availability.

> **Status: pre-alpha (Phase 0 — Foundations).** Nothing here is deployable yet. The current work is architecture decisions, the security baseline, and the CRD API — see the [ADR index](docs/adr/README.md).

Orkano is a control plane for Kubernetes that exposes a PaaS-style workflow: point it at a GitHub repo and it builds and deploys on every push, with TLS, secrets, and a service catalog — without writing YAML. Underneath, everything is a normal Kubernetes object, so you can graduate to `kubectl` at any time without a migration. Security is a product feature, not deployment advice: private by default, least privilege by architecture, builds treated as hostile code.

## What Orkano is not

Deliberately out of scope for v1 (feature requests for these will be closed with a pointer here): multi-cluster federation · provisioning VMs/networks/cloud accounts · a general-purpose Kubernetes admin UI · owning identity beyond one bootstrap admin (real identity is OIDC) · CI/CD beyond build-and-deploy · a paid app marketplace · teams, billing, quotas · metrics dashboards · Windows containers, GPUs, edge.

## Repository layout

| Directory | Contract |
|---|---|
| `/api` | Public Go module (`github.com/orkanoio/orkano/api`): CRD types and the doctor check contract. Never depends on anything heavier than `k8s.io/apimachinery`. Importable by third parties (note: AGPL-3.0-only applies — see [License](#license)). |
| `/operator` | In-cluster operator (controller-runtime). Reconciles Orkano CRs into Kubernetes objects under narrow RBAC. |
| `/cli` | The `orkano` binary: `init`, `proxy`, `doctor`. |
| `/receiver` | Internet-facing webhook receiver. Stateless, no cluster access, no secrets beyond the HMAC key. |
| `/dashboard` | React UI + Go API. Writes Orkano CRDs only; never holds cluster-admin. Arrives in Phase 2. |
| `/docs` | ADRs (`docs/adr`), security docs (`docs/security`), examples (`docs/examples`), the [doctor guide](docs/doctor.md). |
| `/hack` | Dev scripts and throwaway spike code. Not part of the product. |

## Developing

```sh
make all   # lint + test + build, both modules
```

A pinned Go toolchain is fetched automatically via the `toolchain` directive; golangci-lint installs into `./bin`. A [devcontainer](.devcontainer/devcontainer.json) is provided. See [CONTRIBUTING.md](CONTRIBUTING.md) — commits require DCO sign-off (`git commit -s`).

## Security

The threat model, security invariants, and abuse-case catalogue live in [`docs/security/`](docs/security/). Report vulnerabilities per [SECURITY.md](SECURITY.md) — never via public issues.

## License

[AGPL-3.0-only](LICENSE), decided in [ADR-0002](docs/adr/0002-license-agpl-3-0-only-and-dco.md). All directories, including `/api`, carry the same license.
