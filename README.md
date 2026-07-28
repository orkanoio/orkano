# Orkano

Open-source, self-hosted PaaS that makes Kubernetes as easy as Heroku.

Connect a GitHub repository. Orkano builds it on every push, rolls out the new image, and handles TLS for any domain you point at it. No YAML, yet every app is an ordinary Kubernetes object, so `kubectl` still works if you outgrow the UI.

> **Status: alpha.** The engine, dashboard, database catalogs, vault sync and `orkano doctor` are released. Installing onto a cluster you already run (bring your own cluster, via Helm) is in progress and not yet working.

## Install

One fresh server:

- Linux, amd64 or arm64 (tested on Ubuntu 24.04 LTS)
- 2 vCPU, 4 GB RAM, 40 GB disk minimum
- root, or a user with `sudo` (passwordless `sudo` on the multi-server path)
- nothing listening on ports 80, 443, 6443, 2379, 2380 or 10250

Run this on the server:

```sh
curl -fsSL https://github.com/orkanoio/orkano/releases/latest/download/install.sh | sh
```

It verifies checksums, and cosign signatures when `cosign` is present, then preflights the box, installs a hardened k3s, deploys Orkano and prints a one-time dashboard token.

For three servers (etcd quorum), run this from your workstation instead. The kubeconfig points at the first server, so failure-tolerant API access needs a load balancer you provide.

```sh
orkano init \
  --node <server-1> --node <server-2> --node <server-3> \
  --ssh-user root --ssh-key ~/.ssh/id_ed25519 --accept-new-host-key
```

Rather not pipe curl into a shell? Every release ships the bare `orkano` binary, `install.sh`, checksums, cosign signatures and SBOMs.

### First steps

1. Open the SSH tunnel the installer prints, then visit http://localhost:9090. The dashboard is not exposed to the internet by default.

   ```sh
   ssh -L 9090:127.0.0.1:9090 root@<server> \
     '/usr/local/bin/k3s kubectl -n orkano-system port-forward --address 127.0.0.1 svc/orkano-dashboard 9090:80'
   ```
2. Redeem the install token, enroll an authenticator app (required), save the recovery codes.
3. Walk the setup wizard: cluster health, access mode, sign-in, GitHub, first app.

### Bring your own cluster (in progress)

The preflight works today, checking an existing cluster for a supported Kubernetes version, default StorageClass and IngressClass, sufficient RBAC, enforced NetworkPolicies, Pod Security Admission, and AppArmor- and seccomp-capable build nodes.

```sh
orkano preflight --kubeconfig <path>
```

The [Helm chart](charts/orkano/README.md) deploys the substrate, but the in-cluster bootstrap Job that completes it is still being built. Use `orkano init` until then.

## What you get

- Push to deploy from GitHub: signed webhooks, rootless in-cluster builds, digest-pinned rollouts.
- Automatic TLS from Let's Encrypt, with custom domains as first-class objects.
- PostgreSQL and MongoDB catalogs, with optional Pgweb and Mongo Express browsers behind dashboard sign-in.
- A dashboard with live logs, build history and deploys: ClusterIP-only by default, token plus TOTP sign-in, optional OIDC.
- Value-blind secrets the dashboard writes but never reads back, with optional [HashiCorp Vault](docs/vault.md) sync; AWS, GCP, Azure, Keeper, 1Password and Doppler via `kubectl`.
- [`orkano doctor`](docs/doctor.md), a scored hardening report, CI-gateable with `--min-score`.

Out of scope for v1, where feature requests get closed: multi-cluster federation, cloud provisioning, a general-purpose Kubernetes admin UI, identity beyond one bootstrap admin (use OIDC), CI/CD beyond build and deploy, a marketplace, teams, billing, quotas, metrics, Windows containers, GPUs, edge.

## Security

Security is a product feature, not deployment advice: private by default, least privilege by architecture, builds treated as hostile code. Threat model, invariants and abuse cases: [`docs/security/`](docs/security/). Report vulnerabilities per [SECURITY.md](SECURITY.md), never in a public issue.

## Contributing

`make all` lints, tests and builds both modules; the toolchain is pinned and a plain clone needs zero Node (Node 24 only for dashboard work). Start with [CONTRIBUTING.md](CONTRIBUTING.md), and sign off every commit with `git commit -s`. See also the [architecture decisions](docs/adr/) and [example manifests](docs/examples/).

## License

[AGPL-3.0-only](LICENSE), decided in [ADR-0002](docs/adr/0002-license-agpl-3-0-only-and-dco.md). Covers the whole repository, including the importable `api/` Go module.
