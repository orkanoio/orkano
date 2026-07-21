# ADR-0021: Gate unsafe source and automatic-build options explicitly

- Status: Accepted
- Date: 2026-07-21
- Supersedes: ADR-0011

## Context

Orkano's core source path is deliberately narrow: a GitHub App supplies repository identity,
short-lived credentials, verified webhook delivery, and an API re-check that resolves a push to
an immutable commit. Users also need three less-trusted paths: anonymous generic Git, direct ZIP
upload, and Nixpacks-based build-plan detection. Each removes one assurance or adds a large
code-generation dependency, so presenting them as equivalent to the core path would make the
security abstraction dishonest.

The existing `v1alpha1.Source` Go type also made `github` a required value member. An exact-one
source union cannot represent Git or upload honestly while retaining that Go field type. Changing
it to a pointer is a source-breaking change for Go importers, which ADR-0011 would ordinarily
force into a new API version and conversion webhook. However, the stored Kubernetes JSON for every
existing object remains byte-for-byte valid: `source.github` keeps the same name and nested shape,
the validation change only loosens the set of accepted objects, and no stored object needs
conversion. Shipping a second served version and an always-available conversion webhook solely to
adapt a compile-time Go representation would add more upgrade risk than it removes before v1.

## Decision

### Explicit, default-off unsafe gates

Orkano has a typed registry of stable feature IDs. These three IDs are initially defined, all
classified unsafe and disabled unless an installer explicitly enables them:

| ID | Capability | Assurance intentionally lost or added risk |
|---|---|---|
| `source.git` | Anonymous public HTTPS Git source | No GitHub App identity, installation token, verified webhook, or automatic deploy |
| `source.zip` | Direct ZIP source upload | No Git commit or repository provenance; archive parsing and storage become an input surface; every build job can read uploaded source blobs from the shared unauthenticated registry in v1 |
| `build.nixpacks` | Nixpacks plan detection and Dockerfile generation | A larger, maintenance-mode detector/generator runs on hostile source before BuildKit |

There is no master "unsafe mode" boolean. Install surfaces accept an explicit list
(`--enable-unsafe-feature` for the CLI and `features.unsafe` for Helm), validate every ID, reject
unknown or empty IDs, and pass one sorted comma-separated value through
`ORKANO_UNSAFE_FEATURES`. The environment is set directly on the operator and dashboard Pod
templates so changing it causes a rollout. Missing configuration means the empty set.

The dashboard exposes definition metadata and enabled state so disabled choices remain visible but
cannot be selected accidentally. It rejects disabled source/build choices before writing an App.
The dispatcher re-checks before resolving a ref or creating a Build, and the Build reconciler
fails a newly observed gated Build before creating a Job. This defense in depth matters because
the Kubernetes API remains independently usable and feature enablement is installation policy,
not CRD schema. Disabling a gate blocks new work; it does not delete Apps, erase uploaded blobs, or
cancel an already-running Job. Doctor reports any enabled unsafe features and configuration drift
between components as a warning, keeping the hardening posture visible.

The shared registry lives in `internal/features`. `SourceGit`, `SourceZip`, and `BuildNixpacks`
are permanent identifiers. Renaming or reusing an ID would make stored install configuration mean
something different and therefore requires a migration, not a string edit.

### Source and build API

`Source` becomes a CEL-validated exact-one union of optional pointer members:

- `github`: the existing `repo` and optional `ref`, with its wire shape unchanged;
- `git`: an unauthenticated HTTPS URL and optional ref; credentials, query parameters, fragments,
  and non-HTTPS schemes are rejected;
- `upload`: an immutable `sha256:<64 lowercase hex>` OCI-blob digest and optional safe display
  filename.

`subPath` remains common to all source kinds. Generic Git is manual-deploy-only. Its ref is resolved
to an immutable 40-character commit before a Build is created. URL parsing, redirects, DNS results,
and dial targets must remain anonymous HTTPS and publicly routable; redirect or DNS rebinding must
not turn the feature into access to cluster or link-local services.

ZIP uploads are bounded and inspected before storage: absolute/traversing paths, links, duplicate
paths, encrypted entries, excessive entry counts, and compressed or expanded size limits are
rejected. Accepted bytes are stored as a digest-addressed OCI artifact in Orkano's private
in-cluster registry, not in a CR, Secret, ConfigMap, or the metadata database. "Private" here means
network-private to Orkano's build boundary, not confidential between builds: the v1 registry has no
authentication and all build pods must reach it, so a hostile build can enumerate and download any
retained uploaded source artifact. The feature description and UI disclose this explicitly. The
build-side fetch verifies the digest and repeats safe extraction into an ephemeral workspace. A
Build records the 64-character hex digest payload as its immutable revision, so `Build.spec.commit`
loosens from exactly 40 lowercase hex characters to either 40 or 64. Per-build read tokens and
source-repository authorization are deferred with the registry bearer-token work.

`BuildStrategy` adds `Nixpacks` and a matching `nixpacks` configuration member. CEL continues to
make strategy/member mismatches impossible. Nixpacks checks out or consumes the pinned source in
an init step and emits a Dockerfile/build context; the existing rootless BuildKit Job remains the
only image builder. Nixpacks never receives a Docker socket or a long-lived daemon.

Cloud Native Buildpacks are not smuggled behind the Nixpacks name. The normal `pack` workflow
expects a container daemon, which conflicts with the invariant "no Docker socket, ever"; a
Kubernetes-native lifecycle such as kpack adds controllers, CRDs, RBAC, image credentials, and an
independent maintenance surface. It remains a separately designed future feature.

### Narrow pre-v1 API compatibility exception

ADR-0011 is superseded, but its versioning policy remains the rule: within a served alpha version,
fields are additive and optional, validation may only loosen, and a rename, removal, stored-wire
type or semantic change, new required field, or validation tightening forces a new served version
with conversion and the N-1 upgrade promise.

This decision records one narrow exception: changing Go `Source.GitHub` from `GitHubSource` to
`*GitHubSource` in place before v1. Existing JSON/YAML and stored objects do not change or require a
conversion; Go consumers must update composite literals and dereferences when they adopt this
release. Release notes must call out that compile-time break. This exception does not license
further Go API churn, and it does not weaken the promise that stored `v1alpha1` objects remain
mechanically upgradable.

## Consequences

- A default installation retains the existing GitHub + Dockerfile/Static trust boundary and has no
  new source network or archive surface.
- Users can make a deliberate tradeoff per capability, and UI/doctor output can explain the exact
  reduction rather than hiding it behind one broad switch.
- Every component that can turn source into work must consume the same registry and fail closed on
  unknown configuration or a disabled required gate.
- The API module takes one documented compile-time break while retaining stored-object wire
  compatibility; downstream Go users get an obvious compiler failure rather than a silent semantic
  change.
- Uploaded source lifecycle and registry retention need explicit garbage-collection work; disabling
  the feature alone does not destroy source data.
- Enabling ZIP upload accepts cross-build source confidentiality loss until per-build registry read
  authorization exists; this is why the choice remains default-off and visibly unsafe.
- Nixpacks increases image-pinning and multi-architecture verification duties and can be removed
  from an installation without changing BuildKit's core path.

## Alternatives considered

- **Treat all three as ordinary v1 options** — hides materially weaker provenance and expands every
  installation's attack surface even when unused.
- **One `unsafeFeatures=true` switch** — too easy to cargo-cult and impossible for doctor or the UI
  to explain precisely; future additions would silently activate on existing installs.
- **Validate gates only in the dashboard** — `kubectl`, the dispatcher, and already-stored Builds
  bypass it; a compromised or skewed component could still create work.
- **Store ZIP bytes in PostgreSQL, a ConfigMap, or a Secret** — makes the metadata/control plane a
  blob store, pressures etcd, and violates the small-surface and secret-separation architecture.
- **Create `v1alpha2` solely for the Source pointer** — wire conversion is unnecessary, while a
  conversion webhook becomes a new certificate and availability dependency for every CR access.
- **Run `pack` against a Docker socket** — directly violates the hostile-build invariant; rejected
  regardless of convenience.
