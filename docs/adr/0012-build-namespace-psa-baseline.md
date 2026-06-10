# ADR-0012: Build namespace runs at PSA baseline with AppArmor confinement

- Status: Accepted
- Date: 2026-06-11

## Context

INV-02 originally demanded the `restricted` Pod Security level for build pods. The M0.5 spike ([findings](../../hack/spikes/01-buildkit-rootless/FINDINGS.md)) proved that is unreachable for rootless BuildKit on the stack Orkano ships: the `restricted`-mandated RuntimeDefault seccomp profile blocks RootlessKit's user-namespace `clone`; `newuidmap`/`newgidmap` are file-capability binaries that fail under `allowPrivilegeEscalation: false` and a fully dropped bounding set; and containerd's default AppArmor profile denies `mount(2)` *silently*. The spike also proved a working configuration one level down, with every deviation enumerable and compensated.

## Decision

The `orkano-builds` namespace enforces **PSA `baseline`**, and build pods run rootless BuildKit with exactly four deviations from `restricted` (spike attempt F2, the minimal admittable configuration):

1. seccomp field omitted (nil — unconfined on stock k3s),
2. `allowPrivilegeEscalation: true`,
3. `capabilities: {drop: [ALL], add: [SETUID, SETGID]}`,
4. `appArmorProfile: Localhost/orkano-buildkit` — a dedicated profile granting `userns` and `mount` while keeping the rest of the default confinement.

Compensating controls, all capability-probed live in the spike: no ServiceAccount token (`automountServiceAccountToken: false`), default-deny NetworkPolicy with a DNS/registry/443 egress allowlist (enforced by k3s's embedded kube-router — verified by deleting the allowlist and watching the build actually die at the base-image fetch), hard resource limits, `activeDeadlineSeconds` from `Build.spec.timeoutSeconds`, `backoffLimit: 0`, ephemeral Jobs.

Operational consequences the spike surfaced, adopted here:

- `orkano init` loads the `orkano-buildkit` AppArmor profile on every node; a **preflight/doctor check probes that it is loaded** (`build.apparmor-profile-loaded`), because the failure mode without it is a silent mount denial with zero audit entries.
- v1alpha1 build fields freeze as-is: `buildArgs` and `target` are **deferred** (small-surface principle); `timeoutSeconds` default 900 capped at 3600 maps to `activeDeadlineSeconds`; build-job resources default to request 500m/1Gi, limit 2 CPU/4Gi, operator-side.
- Phase 1 uses BuildKit's git-URL context over the already-allowlisted 443 egress; the spike's ConfigMap context does not ship.
- The in-cluster registry speaks TLS from a cluster-internal CA (cert-manager); `registry.insecure` is a spike artifact and never ships.

## Consequences

- INV-02's statement is amended (this ADR is the change-control its rules require): "restricted" becomes "baseline, confined by the dedicated AppArmor profile". The build namespace is consciously the weakest PSA point in the system and carries the densest compensating controls.
- Known environment caveats, untested in the spike and owned by the Phase 4 BYO preflight: clusters with kubelet `seccompDefault` enabled flip nil seccomp back to RuntimeDefault and re-break builds (hardening follow-up: a Localhost *seccomp* profile = RuntimeDefault + unprivileged userns clone, which would also shrink deviation 1); SELinux-based distros behave differently from AppArmor.
- gVisor/Kata RuntimeClass and a tainted build node pool are **not needed** for builds to work; they remain backlog defense-in-depth options.

## Alternatives considered

- **Privileged or root builds / Docker socket** — the failure mode this project exists to avoid.
- **kaniko** — unmaintained; rejected already in planning.
- **gVisor/Kata as the baseline requirement** — heavy node prerequisites for every install to solve a problem the AppArmor profile solves; kept optional.
- **Custom seccomp profile now** — promising hardening (could remove deviation 1), but untested in the spike; deferred rather than shipped unproven.
