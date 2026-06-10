# Spike 1 — Rootless BuildKit Job on k3s: findings

Date: 2026-06-11. Timeboxed to one session (~20 cluster iterations, all attempts run on a live single-node k3s in the `orkano-spike` Lima VM). Every claim below was observed in that session; log excerpts are pasted verbatim.

## Environment

| Component | Value |
|---|---|
| k3s | v1.35.5+k3s1 (single node, flannel CNI, embedded kube-router netpol controller) |
| Kernel | 6.8.0-117-generic, Ubuntu 24.04.4 LTS |
| Arch | arm64 (aarch64, Lima VM, 2 CPU / 4 GiB) |
| containerd | v2.2.3-k3s1 |
| runc | 1.4.2 (libseccomp 2.6.0) |
| cgroup | v2 (`stat -fc %T /sys/fs/cgroup` → `cgroup2fs`) |
| AppArmor | active; 25 profiles enforced incl. `cri-containerd.apparmor.d`; `kernel.apparmor_restrict_unprivileged_userns=1`; `unprivileged_userns_clone=1` |
| BuildKit | `moby/buildkit:v0.30.0-rootless`, `BUILDKITD_FLAGS=--oci-worker-no-process-sandbox` |
| Registry | `registry:3.0.0`, in-cluster, plaintext :5000, `registry.insecure=true` on push |
| kubelet | `seccompDefault` off (probed: nil-seccomp pod shows `Seccomp: 0` in `/proc/self/status`) |

## NetworkPolicy enforcement

**What enforces:** kube-router's NetworkPolicy controller, embedded in k3s. Enforcement is iptables: `KUBE-ROUTER-*` / `KUBE-NWPLCY-*` / `KUBE-POD-FW-*` chains (75 references in `iptables-save` with workloads running), per-pod firewall chains that **REJECT with icmp-port-unreachable** (probes fail fast with "connection refused", not a timeout). The deny rule was attributable by name:

```
-A KUBE-NWPLCY-ZI7X65XICOAJA5TO ... --nflog-prefix "DROP by policy orkano-build-spike/default-deny-all"
```

It is on by default but a single server flag turns it off silently — `k3s server --disable-network-policy`. Orkano's preflight must verify enforcement by probing, never by reading config.

**Proof sequence (all observed):**
1. Baseline, no policies: `probe-client → probe-server:8080` returned the nginx welcome HTML.
2. Applied `03-deny-all.yaml`: same wget failed — first at DNS (`wget: bad address`, egress to kube-dns blocked), then by ClusterIP (`can't connect ... Connection refused`).
3. Egress specifically: from probe-client to the registry in `orkano-spike-infra` **by ClusterIP** (`10.43.229.200:5000/v2/`) failed. The registry namespace has **no** policies, so the block can only be egress enforcement of the client's namespace policy. External egress (`nc 140.82.121.3 443`) also failed.
4. Control: a busybox pod in the policy-free `orkano-spike-infra` namespace reached both the registry (`{}` from `/v2/`) and `140.82.121.3:443` — so the failures above are the policy, not the VM.

**Egress enforcement verdict: enforced, both directions, including DNS.**

**The FQDN gap:** vanilla NetworkPolicy has no FQDN selectors; "allow GitHub only" is not expressible. `04-egress-allowlist.yaml` is the honest approximation: DNS to kube-system, registry:5000, and `0.0.0.0/0:443` minus RFC1918. Options for the real product:
- *GitHub meta-API CIDR list*: real but churns; requires a sync controller and silently rots — poor fit for a solo maintainer.
- *Egress proxy in the build namespace*: hostname-level allowlisting with boring tech; adds a component and TLS-interception or CONNECT-filtering complexity.
- *Cilium FQDN policies*: rejected — replaces the bundled CNI, against "wrap Kubernetes, don't replace it" and the k3s-stock posture.

**Spike recommendation:** ship v1 with deny-all + the proven allowlist (DNS, registry, tcp/443) and document that build pods can reach any HTTPS host. It is enforced, simple, and debuggable at 11pm. Revisit the egress proxy only if FQDN pinning becomes a hard requirement; record the gap in the threat model now.

## Attempt ladder

| Attempt | securityContext delta | PSA level required | Outcome | Error / notes |
|---|---|---|---|---|
| A | none — fully restricted-compliant (RuntimeDefault seccomp, NNP, drop ALL, default apparmor) | restricted (admitted cleanly) | **runtime failure** | `[rootlesskit:parent] error: failed to start the child: fork/exec /proc/self/exe: operation not permitted` — default seccomp blocks `clone(CLONE_NEWUSER)` for unprivileged uid 1000 |
| B | A + `seccompProfile: Unconfined` | **privileged** — baseline *rejects* explicit Unconfined: `violates PodSecurity "baseline:latest": seccompProfile (container "buildkit" must not set securityContext.seccompProfile.type to "Unconfined")` (job created; pods FailedCreate) | runtime failure | `failed to setup UID/GID map: newuidmap 20 [0 1000 1 1 100000 65536] failed: : fork/exec /usr/bin/newuidmap: operation not permitted` |
| C | B + apparmor `unconfined` (annotation) | privileged | runtime failure | identical newuidmap EPERM — apparmor was not the blocker at this rung |
| D | C + `procMount: Unmasked` + `hostUsers: false` | privileged | runtime failure | identical newuidmap EPERM. Side-finding: k3s 1.35 starts `hostUsers: false` pods fine |
| E | nil seccomp + `allowPrivilegeEscalation: true` + caps `add: [SETUID, SETGID]`, default apparmor | **baseline** (admitted cleanly) | runtime failure, much further | `[rootlesskit:child ] error: failed to share mount point: /: permission denied` — cri-containerd default AppArmor profile denies `mount(2)`; denial is **silent** (zero `apparmor=DENIED` audit entries; consistent with the explicit `deny mount,` rule in containerd's default profile template — explicit denies are not audited; profile is generated in-memory, nothing on disk to read) |
| F1 | E + `appArmorProfile: Unconfined` | privileged | **SUCCESS, 8 s** | full build + push; notably `apparmor_restrict_unprivileged_userns=1` did *not* break unconfined userns creation on this kernel/runtime combo |
| F2 | E + `appArmorProfile: {Localhost, orkano-buildkit}` (custom profile = default-ish + `userns,` + `mount,`) | **baseline** (admitted cleanly, zero warnings) | **SUCCESS, 7 s** | the minimal admittable configuration |

The B/C/D failures share one root cause the authored ladder never varied: `newuidmap`/`newgidmap` in the rootless image are **file-capability binaries, not setuid** (probed: `-rwxr-xr-x root root`), and exec of a file-caps binary fails with EPERM when NoNewPrivs is set (`allowPrivilegeEscalation: false`) or when SETUID/SETGID are absent from the bounding set (`drop: [ALL]`).

**Minimal deviation from restricted that works (= F2, PSA baseline):**
1. seccomp: field omitted — nil is baseline-legal and means *unconfined* on stock k3s (kubelet `seccompDefault` off). Caveat: a cluster that enables `seccompDefault` flips nil to RuntimeDefault and re-breaks attempt-A-style. Hardening follow-up: a Localhost *seccomp* profile (RuntimeDefault plus unprivileged `clone`/`unshare` userns) — not tested in this spike.
2. `allowPrivilegeEscalation: true` (baseline-legal).
3. `capabilities: {drop: [ALL], add: [SETUID, SETGID]}` — both in baseline's allowed add-list.
4. `appArmorProfile: Localhost/orkano-buildkit` (baseline-legal; Unconfined is not). Node prerequisite: the profile must be loaded (`apparmor_parser -r`); without it the pod fails to start, and with the default profile builds die at a *silent* mount denial — the nastiest failure mode found in this spike.

What this means: **`restricted` is unreachable for rootless BuildKit on this stack — but `baseline` is reachable**, and every deviation is enumerable and compensated (see verdict).

## Egress allowlist capability probe

INV-02 probe, run around the working F2 job:

- **Allow leg:** with `04-egress-allowlist.yaml` in place, the build pulled `alpine:3.22` from Docker Hub over 443 (`#2 load metadata ... DONE 2.5s`, layer `4.14MB / 4.14MB 0.6s done`) and pushed to the in-cluster registry (`#6 pushing manifest for registry.orkano-spike-infra.svc.cluster.local:5000/spike:f1 ... done`).
- **Deny leg:** deleted the allowlist (deny-all then governs `app=buildkit`), re-ran the identical job → failed at the base-image fetch:
  `dial tcp: lookup production.cloudfront.docker.com on 10.43.0.10:53: read udp 10.42.0.21:44148->10.43.0.10:53: read: connection refused`
- **Restore leg:** re-applied the allowlist, re-ran → `succeeded=1` in 7 s.
- **Artifact check:** registry catalog from inside the VM: `{"name":"spike","tags":["f1","f2"]}`.

The policy demonstrably constrains the build pod, not just bystanders.

## Performance

- Wall time, working job (start → completionTime): **8 s (F1), 7 s (F2), 6 s (re-run)** — trivial 2-step Dockerfile, base image re-pulled each run (per-job emptyDir cache), push to local registry.
- Base image fetch: metadata 2.5 s; 4.14 MiB alpine layer in 0.6 s through the allowlist.
- First-ever run additionally pulled `moby/buildkit:v0.30.0-rootless` (not separately timed; folded into attempt A).
- Peak pod memory: **43 MiB** (polled the pod's cgroup `memory.current` at 250 ms during a run). `kubectl top` never caught the pod — metrics-server works on k3s but its scrape interval exceeds the build duration.
- Caveat: a trivial build proves the floor, not the envelope. Real builds (compilers, npm) are minutes and GiBs; the 1 CPU / 2 GiB limits used here never came close to binding.

## Recommended CRD build fields

- **`timeoutSeconds`** — default **900**, mapped 1:1 onto Job `activeDeadlineSeconds` (proven mechanism). Trivial build was 7 s; real cold builds need minutes; 15 min is generous without letting a wedged build squat the node. Cap user overrides (e.g. 3600).
- **Resources** — defaults: request `500m` / `1Gi`, limit `2` CPU / `4Gi`. The spike's 1 CPU / 2 GiB sufficed at 43 MiB peak, but that floor is unrepresentative; OOM-kill at the limit is the expected failure mode for heavy builds and must be surfaced in build logs/status. Make per-app overridable later; ship fixed defaults in v1alpha1.
- **`buildArgs` / `target`** — **defer from v1alpha1.** Neither was needed to prove the core claim; each widens the surface (arbitrary user strings into the frontend, more states to surface). Add on demand behind a deliberate decision (small-surface principle).
- **Context delivery** — the ConfigMap context is spike-only (1 MiB cap, no git history). Phase 1 should use BuildKit's git context (`--opt context=https://github.com/...#<ref>`), which rides the already-proven tcp/443 egress rule and removes the ConfigMap entirely.
- **Registry TLS posture** — the spike used `registry.insecure=true` over plaintext :5000. **The product must not.** Recommend: cert-manager-issued cert from a cluster-internal CA on the registry Service, CA bundle projected into the build pod (BuildKit registry config). TLS-or-nothing for v1; `insecure` stays a spike artifact.

## Verdict vs PLANNING risk #2

**GO.** Rootless BuildKit builds are viable on stock k3s at **PSA `baseline`** with exactly these deviations from `restricted`: effectively-unconfined seccomp (nil field), `allowPrivilegeEscalation: true`, `+SETUID/+SETGID`, and a custom Localhost AppArmor profile granting `userns` + `mount` (node prerequisite on AppArmor-4 distros like Ubuntu 24.04; Orkano must install/load it — node-setup step or DaemonSet — and preflight-check it, because the failure without it is a *silent* mount denial).

Fallbacks (tainted build node pool, gVisor/Kata) are **not needed** to make builds work; keep them in the threat model as defense-in-depth options only. Since `baseline < restricted`, this **requires the build-namespace PSA exception ADR (ADR-0012)** with compensating controls — all proven live in this spike: `automountServiceAccountToken: false`, default-deny NetworkPolicy + minimal egress allowlist (capability-probed both ways), resource limits, `activeDeadlineSeconds`, `backoffLimit: 0`. Untested here and flagged: SELinux distros (RHEL-family) and clusters with kubelet `seccompDefault` enabled behave differently; both belong in the ADR's assumptions.

## Teardown

- `limactl delete orkano-spike` removes the VM and everything in it (cluster, images, the loaded AppArmor profile).
- Keep (all in `hack/spikes/01-buildkit-rootless/`): the numbered manifests, the attempt jobs A–F2, `apparmor-orkano-buildkit.profile`, `run.sh`, and this file. `job-f2-localhost-apparmor.yaml` + the AppArmor profile are the direct inputs to the Phase-1 build Job template and ADR-0012.
