# ADR-0001: Record architecture decisions

- Status: Accepted
- Date: 2026-06-10

## Context

Orkano is built by one person in spare time. The biggest documented risks to the project are scope creep and the solo bus factor (PLANNING.md risks #1 and #9): a decision made at 11pm and never written down is a decision the project loses. The Phase 0 exit criterion is explicit — no open decision may lack an ADR.

## Decision

We record every decision that touches architecture, public API, security posture, or scope as an Architecture Decision Record in `docs/adr/`.

- Format: the lean template in [template.md](template.md) — Context, Decision, Consequences, Alternatives considered. No decision matrices or stakeholder sections.
- Filename: `NNNN-kebab-case-slug.md`. Numbers are assigned when the decision is first proposed; TASKS.md pre-reserves 0001–0008, so gaps and out-of-order dates are expected.
- Statuses: `Proposed`, `Accepted`, `Superseded by ADR-NNNN`. An accepted ADR is immutable — to change course, write a new ADR that supersedes it.
- The index lives in [README.md](README.md) and is updated in the same commit as the ADR it lists.

## Consequences

- Every "why is it like this?" question has a findable answer, which is what makes the project survivable through a maintainer hiatus and reviewable by outside contributors.
- Small overhead on every significant decision; accepted — the template is deliberately short enough to fill in under ten minutes.
- Superseding instead of editing creates some duplication over time; accepted in exchange for an honest history.

## Alternatives considered

- **Full MADR** — decision-driver matrices are enterprise ceremony a solo maintainer will not keep up.
- **Decisions in PLANNING.md only** — PLANNING.md states current intent; it cannot carry the why-not-X history without bloating.
- **No formal records** — directly contradicts the Phase 0 exit criterion and risk #9 mitigation.
