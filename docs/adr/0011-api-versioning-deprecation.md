# ADR-0011: API versioning and deprecation policy

- Status: Accepted
- Date: 2026-06-11

## Context

v1alpha1 signals instability, but early adopters' clusters will hold real stored objects, and PLANNING risk #6 (API churn breaking early adopters) is rated medium likelihood, high impact. The policy for what changes are allowed, and what happens when they aren't, has to exist before the first object is stored.

## Decision

**Within v1alpha1**, only additive changes ship: new fields must be optional and either defaulted or safely absent, and validation may only *loosen* in place. A version bump is forced by any rename, removal, type change, semantic change to an existing field, new required field, or validation tightening that could invalidate a stored object.

**Conversion:** while exactly one version exists, no conversion webhook exists (nothing to convert, nothing to operate). The first additional version (v1alpha2 or v1beta1) ships *with* a conversion webhook compiled into the operator binary. The storage version flips one release after the new version ships, and the old version stays served for at least one further minor release.

**Promise to early adopters:** CRs written against v1alpha1 will always be upgradable by `orkano upgrade` — that is the mechanical meaning of the N-1 guarantee. A served version is removed no sooner than two minor releases after its deprecation is announced in release notes.

## Consequences

- Field mistakes in v1alpha1 are lived with until a version bump, which is the pressure that justified design-by-example before Go types.
- The operator binary eventually grows a conversion webhook — accepted as the price of the upgrade promise, deferred until a second version actually exists (consistent with ADR-0010's no-webhook stance, which this supersedes only at that point and only for conversion).
- Release notes become a load-bearing artifact: deprecation clocks start there.

## Alternatives considered

- **Free churn while alpha** — technically licensed by Kubernetes convention, but it spends early adopters' trust, the launch asset Orkano needs most.
- **Promise stability (jump to v1beta1 now)** — dishonest before the operator has reconciled a single real workload.
- **External conversion service** — a separate deployment to operate; the operator binary already has the types.
