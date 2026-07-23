# `orkano doctor`

`orkano doctor` checks a running Orkano install's health and hardening and
prints a report plus a **hardening score**. It is the third consumer of the
shared check framework (the install preflight and the onboarding wizard are the
other two), so results look and gate the same everywhere.

```sh
orkano doctor
orkano doctor --json --min-score 80     # CI gate
sudo orkano doctor --local              # on the server: adds node checks
```

The cluster is read through the kubeconfig `orkano init` wrote. Resolution
order: `--kubeconfig`, then `$KUBECONFIG`, then `./orkano.kubeconfig`.

## What it checks

| Check | Severity | Passes when | Skips when |
|---|---|---|---|
| `platform.components-ready` | critical | every core platform workload in `orkano-system` — the operator, receiver, registry and dashboard Deployments and the Postgres StatefulSet — has at least one ready replica | never — a missing or unready component **fails**; re-run `orkano init` to repair a partial install |
| `exposure.dashboard-not-public` | critical | the dashboard Service is ClusterIP-only, has no external IPs, and nothing in `orkano-system` (Ingress or Traefik IngressRoute) routes to it — the INV-05 private-by-default contract | the dashboard Service does not exist |
| `tls.certificate-expiry` | warning | no platform certificate is expired, within 14 days of expiry, more than 7 days past its renewal time, or still unissued after 24 hours | never — a missing cert-manager or zero certificates **fails**, since `orkano init` always installs the platform PKI |
| `backup.etcd-snapshot-age` | warning | the newest usable etcd snapshot is younger than 25 hours (twice the default 12-hour schedule, plus slack) | the cluster does not run embedded etcd, is younger than one snapshot window, or has no k3s snapshot-record CRD (non-k3s, or a k3s version predating it) |
| `net.networkpolicy-enforced` | critical | a live capability probe confirms the build-namespace default-deny egress actually blocks traffic | never |
| `secrets.store-health` | warning | every SecretStore and ExternalSecret in `orkano-apps` reports Ready (ESO's own live validation), every *periodically-refreshed* sync's `refreshTime` is within twice its refresh interval (sync-once objects — `refreshPolicy: CreatedOnce`/`OnChange` or `refreshInterval: 0s` — are exempt from freshness), and every target Secret exists; all unhealthy objects are reported in one run | the External Secrets Operator is not installed (enable with `orkano init --secrets-vault`), or it is installed with no stores or syncs configured |
| `features.unsafe-disabled` | warning | the operator and dashboard agree that no default-off unsafe source or build feature (generic Git, ZIP upload, Nixpacks) is enabled | the operator and dashboard Deployments are not installed |
| `build.apparmor-profile-loaded` | critical | the `orkano-buildkit` AppArmor profile is loaded in enforce mode on this node (`--local` only, requires root) | not registered without `--local` |

`net.networkpolicy-enforced` is the one check that *creates* resources: three
short-lived canary pods in `orkano-builds` (a labeled control that must
connect, and unlabeled canaries that must be blocked). They are removed
afterwards; leftovers from an interrupted run are swept on the next one.

A check that cannot be determined (an unreachable API, an unreadable resource)
reports **error**, never pass — unknown state never counts as hardened.

## The hardening score

The score is a severity-weighted percentage of the applicable checks that
passed: a critical control is worth far more than an informational one, so the
number tracks real hardening rather than a raw check count.

- **Skipped** (not-applicable) checks are excluded entirely.
- **Errored or blocked** checks count against the score exactly like failures.
- An install with any unhardened check never reads 100, and one with any
  hardened check never reads 0 — the extremes are honest.

## Exit codes and CI gating

| Code | Meaning |
|---|---|
| 0 | every critical check passed (and the `--min-score` gate, if set, was met) |
| 1 | a critical check failed, or the score fell below `--min-score` |
| 2 | a critical check could not be determined |

When the score gate and an indeterminate check both apply, exit 1 takes
precedence over 2 — a gate miss is definitively actionable, the same way a
real critical failure outranks an indeterminate one. And because errored or
blocked checks count against the score, a `--min-score` failure does not
always mean a check definitively failed; a CI consumer that needs that
distinction should read the per-check outcomes in the `--json` body rather
than the bare exit code.

Warnings and info checks never change the exit code on their own —
`--min-score <1-100>` is how a warnings-only regression (an aging backup, a
stuck certificate renewal) gates CI:

```sh
orkano doctor --json --min-score 80 || echo "hardening regressed"
```

`--json` emits a stable envelope carrying every result, the score, the
threshold, and the final exit code, so a CI consumer needs no second pass.

## `--fix`

`--fix` applies the automatic fix of every **failing** check that has one, then
re-runs every check and reports which fixes resolved. Fixing is single-pass: a
check that only becomes fixable after another fix lands needs a second run.
Checks that errored or were blocked are never auto-remediated — doctor does not
"fix" what it cannot see. The plain report marks fixable failures and suggests
the flag.

## In the dashboard

The dashboard surfaces the same checks on its Doctor page, running them
per-request as the impersonated read-only viewer rather than the admin
kubeconfig. It reports the **read-only subset**: `net.networkpolicy-enforced` is
omitted because it creates pods (the viewer holds no such grant), and
`secrets.store-health` runs value-blind — it cannot read a target Secret to
confirm existence, so that one leg is verified only by the CLI `orkano doctor`.
Per-check outcomes, messages and remediations match the CLI report; the score
and status use the same semantics but are computed over the read-only subset, so
they can differ from a CLI run — in particular, a failing network-policy probe
lowers the CLI score and exit code without affecting the dashboard's. Run
`orkano doctor` for the full set, including the live network-policy probe and the
target-Secret existence check.
