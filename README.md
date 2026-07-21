# Orkano

Open-source, self-hosted PaaS that makes Kubernetes as easy as Heroku.

Point Orkano at a GitHub repository and it builds and deploys on every push — TLS, custom domains, databases, secrets, and live logs included, without writing YAML. Underneath, everything is a normal Kubernetes object, so you can graduate to `kubectl` at any time without a migration. Security is a product feature, not deployment advice: private by default, least privilege by architecture, builds treated as hostile code.

> **Status: alpha.** The engine, dashboard, database catalog, secrets integration, and `orkano doctor` are complete and released. Installing onto an existing cluster (bring-your-own) via Helm is the current milestone — its preflight works today, the chart is not yet a working install.

## Install

On one fresh Ubuntu 24.04 LTS server (as root · ≥2 vCPU · 4 GB RAM · 40 GB disk · nothing listening on 80/443/6443):

```sh
curl -fsSL https://github.com/orkanoio/orkano/releases/latest/download/install.sh | sh
```

The installer verifies checksums (and, when `cosign` is present, signatures) before executing anything, runs preflight checks, installs a hardened k3s, deploys Orkano — operator, dashboard, build system, internal registry, cert-manager — and prints a one-time dashboard token exactly once.

For a highly-available three-server cluster, run from your workstation over SSH instead:

```sh
orkano init \
  --node <server-1> --node <server-2> --node <server-3> \
  --ssh-user root --ssh-key ~/.ssh/id_ed25519 --accept-new-host-key
```

Prefer not to pipe curl into sh? Every release ships the bare `orkano` binary, `install.sh`, checksums, cosign signatures, and SBOMs — download, verify, read, then run.

### First steps

1. Reach the dashboard through the SSH tunnel the installer prints — it is never exposed to the internet by default:
   `ssh -L 9090:127.0.0.1:9090 root@<server> '/usr/local/bin/k3s kubectl -n orkano-system port-forward svc/orkano-dashboard 9090:80'` → http://localhost:9090
2. Redeem the install token, enroll the required authenticator, save the recovery codes.
3. Walk the setup wizard: cluster health → access mode → sign-in → GitHub → your first app.

### Bring your own cluster (in progress)

`orkano preflight --kubeconfig <path>` probes an existing cluster for everything Orkano needs — enforced NetworkPolicies, Pod Security Admission, AppArmor-capable build nodes, a default StorageClass, sufficient RBAC. The Helm chart under `charts/orkano/` installs the substrate but does not yet produce a working installation; the in-cluster bootstrap job that completes it is being built now.

## What you get

- Push-to-deploy from GitHub — a webhook doorbell, rootless in-cluster builds, digest-pinned rollouts
- Automatic TLS via Let's Encrypt, custom domains as first-class objects
- PostgreSQL and MongoDB catalogs, with optional session-guarded Pgweb / Mongo Express admin UIs
- A dashboard with live logs, build history, and one-click manual deploys — ClusterIP-only by default, bootstrap-token + TOTP auth, optional OIDC SSO
- Value-blind secrets (the dashboard can write them, never read them back) and optional external vault sync
- `orkano doctor` — a scored hardening check, CI-gateable with `--min-score`

## What Orkano is not

Deliberately out of scope for v1 (feature requests for these will be closed with a pointer here): multi-cluster federation · provisioning VMs/networks/cloud accounts · a general-purpose Kubernetes admin UI · owning identity beyond one bootstrap admin (real identity is OIDC) · CI/CD beyond build-and-deploy · a paid app marketplace · teams, billing, quotas · metrics dashboards · Windows containers, GPUs, edge.

## Repository layout

| Directory | Contract |
|---|---|
| `/api` | Public Go module (`github.com/orkanoio/orkano/api`): CRD types and the doctor check contract. Never depends on anything heavier than `k8s.io/apimachinery`. Importable by third parties (note: AGPL-3.0-only applies — see [License](#license)). |
| `/operator` | In-cluster operator (controller-runtime). Reconciles Orkano CRs into Kubernetes objects under narrow RBAC. |
| `/cli` | The `orkano` binary: `init`, `preflight`, `doctor`. |
| `/receiver` | Internet-facing webhook receiver. Stateless, no cluster access, no secrets beyond the HMAC key. |
| `/dashboard` | React UI + Go API. Writes Orkano CRDs only; reads impersonate a pinned view-only identity; never holds cluster-admin. |
| `/charts` | The Helm chart for bring-your-own-cluster installs (in progress). |
| `/docs` | ADRs (`docs/adr`), security docs (`docs/security`), examples (`docs/examples`), the [doctor guide](docs/doctor.md), the [vault guide](docs/vault.md). |
| `/hack` | Dev scripts and throwaway spike code. Not part of the product. |

## Developing

```sh
make all   # lint + test + build, both modules
```

A pinned Go toolchain is fetched automatically via the `toolchain` directive; golangci-lint installs into `./bin`. The dashboard SPA needs Node ≥24 only when you work on it (`make verify-web`); a plain clone builds and tests with zero Node. See [CONTRIBUTING.md](CONTRIBUTING.md) — commits require DCO sign-off (`git commit -s`).

## Security

The threat model, security invariants, and abuse-case catalogue live in [`docs/security/`](docs/security/). Report vulnerabilities per [SECURITY.md](SECURITY.md) — never via public issues.

## License

[AGPL-3.0-only](LICENSE), decided in [ADR-0002](docs/adr/0002-license-agpl-3-0-only-and-dco.md). All directories, including `/api`, carry the same license.
