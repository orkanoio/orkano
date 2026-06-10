# Security Policy

Security is a product feature in Orkano — private by default, least privilege by architecture — and this policy is how you report flaws in it.

## Reporting a vulnerability

**Primary channel:** [GitHub Private Vulnerability Reporting](https://github.com/orkanoio/orkano/security/advisories/new) on `github.com/orkanoio/orkano`.

**Fallback:** contact@orkano.io. Note: the project domain registration is still pending — until it resolves, use private reporting on GitHub.

**Never open a public issue for a security problem.** A PaaS control plane holds Git credentials and runs code from repositories by design; a public report puts every install at risk before a fix exists.

Please include:

- The Orkano version or commit hash you tested against.
- The affected component (dashboard, webhook receiver, operator, build jobs, registry, installer/CLI).
- The impact as you understand it — what an attacker gains. If the flaw breaks one of the documented security invariants (INV-01 through INV-08 in `PLANNING.md`), say which one; that fast-tracks triage.
- Reproduction steps or a proof of concept. A minimal repro is worth more than a long writeup.

## Scope

**In scope:**

- Orkano source code in this repository (dashboard, webhook receiver, operator, CLI, build job orchestration).
- Published Orkano container images and release artifacts.
- The installer (`orkano init`) and the cluster configuration it produces.

**Out of scope:**

- Kubernetes and k3s themselves — report upstream.
- The host OS of your nodes.
- Vulnerabilities in third-party dependencies — report upstream.

Exception: if Orkano's defaults or configuration make an out-of-scope component exploitable (say, our k3s flags disable a protection, or our manifests grant a dependency more privilege than it needs), that is an Orkano bug. Report it here.

## Supported versions

| Version | Supported |
|---------|-----------|
| Pre-1.0 releases | Latest release only |
| v1.0 and later | N and N-1 |

This matches the documented upgrade guarantee: `orkano upgrade` supports N-1, so security fixes do too.

## Response targets

| Stage | Target |
|-------|--------|
| Acknowledgement | within 7 days |
| Triage + severity assessment | within 14 days |
| Fix for critical issues | best effort, targeting 30 days |
| Fix for everything else | next release |

Honest caveat: Orkano is a solo, spare-time project. These are targets we take seriously, not an SLA contract.

## Coordinated disclosure

We follow a 90-day default disclosure window from the initial report, shortened if a fix ships earlier or the issue is being exploited in the wild, and extendable by mutual agreement if a fix genuinely needs more time. Advisories are published as GitHub Security Advisories (GHSA) on this repository. Reporters are credited in the advisory unless they ask not to be.

## Safe harbor

Good-faith security research against your own Orkano installs is welcome and will never result in legal action from this project — that includes fuzzing, probing the invariants, and trying to break the build sandbox on infrastructure you control. Do not test against instances you don't own or operate; other people's clusters are other people's clusters. There is no bug bounty — the only rewards on offer are credit and gratitude.
