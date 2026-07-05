# ADR-0017: On-box quick install via a local-exec runner and a thin, verifying installer

- Status: Accepted
- Date: 2026-07-04

## Context

Install today is `orkano init` over SSH (M1.5): the operator runs the CLI on a control host (their laptop), it SSHes into 1 or 3 nodes with pinned host keys, and bootstraps hardened k3s + deploys the Orkano components remotely. The user never runs anything on the box.

The competing self-hosted PaaS category (Coolify, Dokploy, CapRover) onboards differently: SSH into a fresh VPS, paste a `curl https://get.orkano.io | sh` one-liner, the installer runs on the box, and you get a dashboard. That is the friction the PRD's "side-project ship in 5 minutes" and the "5-minute first deploy demo reproducible by a stranger" success criterion are chasing. This ADR adds that on-box path without giving up what makes Orkano different.

Two PRD constraints frame the whole decision:

1. **Provisioning is a non-goal, but bringing a box is not.** `orkano init` "installs Kubernetes onto Linux machines the user already has." A `curl | sh` on a VPS the user already rented does not create infrastructure, so it is in scope — this is a new install *transport*, not a new *capability class*.
2. **The category is defined by exposed panels; Orkano's differentiator is that it is not.** INV-05 / ADR-0004: the dashboard ships ClusterIP-only and is never publicly reachable by default; `--expose public` refuses until SSO/MFA is configured. "Paste a script, get a public dashboard link" is precisely the anti-pattern the product positions against (Shodan is full of that class of panel). So the on-box installer must produce a *private* way in, not a public URL.

A third constraint is a supply-chain one the PRD states outright: "our installer and images are an attack vector for every downstream user." Phase 5 already reserves *"checksummed installer; the non-`curl | bash` path documented prominently."* This ADR pulls that forward and keeps its security posture intact rather than shipping a fat, unauditable bash installer.

## Decision

1. **One engine, two transports.** The bootstrap engine is already written against an abstract runner — `internal/k3s`, `internal/install`, `internal/nodeprep` each define a `Runner`, and `internal/preflight` an `Executor`, all with the identical method `Run(ctx, cmd) (ssh.Result, error)`. Only `*ssh.Client` satisfies it today. We add a **local-exec runner** in a new `internal/localexec` package that runs each command on the host via `os/exec` (`sh -c "<cmd>"`) and returns the same `ssh.Result{Stdout, Stderr, ExitStatus}` shape, mapping a non-zero exit to a `Result` (not a Go error) exactly as the SSH transport does. No engine logic forks; the on-box path is a transport swap. The command strings the engine emits (base64|tee file writes, the version-pinned `curl … | sh` k3s install, `apparmor_parser`, kubectl) are already injection-safe by construction, so running them through `sh -c` locally introduces no new escaping surface.

2. **A dedicated `orkano init --local` mode, single-node only.** `--local` runs the whole engine — local preflight, k3s bootstrap (embedded etcd, secrets-encryption, audit, CIS templates), AppArmor node-prep, component deploy, registry wiring — against the local-exec runner. It takes no `--node`, `--ssh-*`, host-key, or TOFU flags: there is no remote to reach or pin. It requires root: the installer script runs the verified binary under `sudo` (download + verification stay unprivileged), and the binary itself refuses cleanly with the reason when not root. It is **single-node only in v1**: a box can bootstrap itself, but standing up a 3-node HA cluster still needs the other two nodes reached over SSH, so multi-node HA stays the `orkano init --node … --node … --node …` path. This is an honest tradeoff — the easiest install is also the least highly-available — and it is stated in the CLI output and docs.

3. **The installer script is thin and verifying — the audited logic stays in Go.** `install.sh` (served at `get.orkano.io`) does only: detect OS/arch → download the version-pinned, signed `orkano` binary from GitHub Releases → **verify its checksum, and its cosign signature when the release provides one** → exec `orkano init --local`. It fails closed on any verification failure. It carries no bootstrap logic of its own; everything security-critical lives in the reviewed Go binary and its unit/e2e tests. The non-pipe path is documented as prominently as the one-liner (`curl -fsSLO get.orkano.io/install.sh` → read it → `sh install.sh`), satisfying the Phase 5 "non-`curl | bash` path documented prominently" requirement.

4. **The dashboard stays private; the installer prints a localhost path in, never a public URL (INV-05 upheld).** The on-box install performs no exposure. On success it prints (a) the one-time bootstrap token exactly once (ADR-0003) and (b) a copy-paste command that reaches the ClusterIP-only dashboard from the operator's laptop over an SSH local-forward, landing them at `http://localhost:<port>`. The clean one-command form is `orkano proxy` once it exists (ADR-0004's named default); until then the installer prints the explicit SSH tunnel. No `--expose public`, no `sslip.io`, no NodePort — exposure remains a deliberate, wizard-gated choice made later, and the "dashboard link" the user asked for is a localhost link reached in one command, not an internet-facing panel.

5. **Supply chain and air-gap posture are unchanged.** The binary is already cosign-signed and SBOM'd by goreleaser; the script pins a version and verifies the artifact. The on-box `curl | sh` needs outbound internet (binary download + the k3s installer + image pulls), so it is not an air-gapped path; the pre-staged-binary offline install stays backlog (TASKS.md M1.5 line), and the manual download-and-verify path is documented for the security-conscious.

## Consequences

- **New public attack surface: `install.sh` plus a CLI mode.** Both are entry points for every downstream user, which is exactly why the script is a thin fetch+verify+exec wrapper over reviewed Go rather than a fat bash installer, and why verification is mandatory and fail-closed.
- **The engine gains a second transport for free, and keeps one code path.** Every CIS template, the AppArmor profile load, the restricted-PSA component deploy, the secrets generation, and their tests apply unchanged to the on-box path. A future third transport (e.g. an agent) would satisfy the same interface.
- **Single-node on-box; HA stays SSH.** The `curl | sh` box is a single point of failure by construction. Users who want the PRD's HA differentiator run the 3-node SSH path. Doctor (M3.2) can flag single-node as a warning, not a failure.
- **Root requirement is explicit.** Bootstrapping k3s + loading an AppArmor profile needs root; the installer script execs the verified binary under `sudo` (the script never re-runs itself — download + verification stay unprivileged), and the binary refuses with a clear message when not root (PRD: "errors are documentation").
- **Doctor closes the loop.** After install, `orkano doctor` proves the box is healthy and hardened and `--fix`es what it safely can; the installer's final output points at it, so the "is my install OK?" question has a first-class answer.
- **Reaching a ClusterIP service from outside always needs a hop.** The localhost-link UX depends on the SSH tunnel (or `orkano proxy`); a user with no SSH access to their own box cannot reach the dashboard, which is the correct failure mode for a private-by-default panel.

## Alternatives considered

- **Fat shell installer (all bootstrap logic in bash).** The Coolify shape. Rejected: it moves security-critical logic out of the reviewed, tested Go engine into an unauditable, hard-to-maintain script, and it would duplicate the entire k3s/hardening path.
- **`orkano init` SSH-ing to `localhost`.** Reuses the existing transport unchanged, but requires a running `sshd` and a pinned host key on the box for no benefit. Rejected for the simpler local-exec runner, which also drops the sshd dependency.
- **A separate `orkano-installer` binary.** Rejected: one binary, one engine. `--local` is a transport selection on the existing `init`, not a second program to sign, ship, and keep in sync.
- **Public dashboard link by default (NodePort / `sslip.io` / LoadBalancer).** The fastest "Heroku feel" and what the competition does. Rejected outright: it violates INV-05 and is the exact exposed-panel anti-pattern the product's security architecture is built to avoid. The token-redeem window would sit on a public URL before TOTP is even set.
- **Multi-node HA from the on-box installer** (the box SSHes out to two peers). Rejected for v1: it re-introduces the SSH transport, host-key pinning, and multi-node orchestration into what should be the simplest path. The SSH `orkano init` already does HA well; the on-box path stays single-node and honest about it.
