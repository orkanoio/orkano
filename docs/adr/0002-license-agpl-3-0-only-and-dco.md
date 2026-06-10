# ADR-0002: License under AGPL-3.0-only with DCO sign-offs

- Status: Accepted
- Date: 2026-06-10

## Context

The license is the single most irreversible decision in the project: once external contributions land, relicensing requires every contributor's consent. PRD.md carried Apache-2.0 as a working default, flagged for conscious confirmation before the repo goes public. The product category sharpens the question — a self-hosted PaaS control plane is exactly the kind of software a company can take, harden privately, and resell as a hosted service without contributing anything back. Network copyleft is the only license mechanism that addresses that.

## Decision

The entire repository, including the `/api` module, is licensed **AGPL-3.0-only**. Copyright is held as "The Orkano Authors". Contributions are accepted under the **Developer Certificate of Origin** — every commit carries a `Signed-off-by` line (`git commit -s`); no CLA.

`-only` rather than `-or-later`: license terms stay exactly as written today instead of delegating future terms to the FSF. This is the convention of comparable infrastructure projects (Grafana, MinIO).

## Consequences

- Anyone offering Orkano as a network service must publish their modifications — the reseller scenario is closed, and the security architecture (the project's differentiator) cannot be forked into a proprietary product.
- Corporate adoption friction is consciously accepted: many legal departments blanket-ban AGPL, which will cost some of the "engineering teams" secondary persona and forecloses CNCF donation (Apache-2.0 required).
- The `/api` module's "importable by third parties" contract is narrowed: it remains importable, but importing Go code under AGPL obligates the importer to AGPL-compatible licensing. Third-party closed-source tooling will not import it. The Apache-2.0 carve-out for `/api` (the MinIO pattern) was considered and **deliberately rejected** in favor of one license everywhere — maximal copyleft and a single, simple story.
- With DCO instead of a CLA, the project keeps zero legal infrastructure and minimal contributor friction, and gives up the option to dual-license commercially later: relicensing would require consent from all past contributors. Accepted.

## Alternatives considered

- **Apache-2.0** — maximal adoption and CNCF compatibility, but leaves the hosted-reseller door open; rejected as the threat is specific to this product category.
- **AGPL-3.0 server + Apache-2.0 `/api`** — preserves third-party importability (MinIO pattern); rejected for license uniformity and maximal copyleft.
- **AGPL-3.0-or-later** — delegates future license terms to the FSF; rejected for predictability.
- **CLA** — preserves dual-licensing; rejected: signup friction works against the 10-contributor launch goal, and administering legal paperwork does not fit a solo maintainer.
