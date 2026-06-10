# ADR-0008: Dashboard on React, TypeScript, Vite, TanStack Query, Tailwind, shadcn/ui

- Status: Accepted
- Date: 2026-06-11

## Context

The original planning notes proposed this stack; this ADR confirms it through the lens that actually matters for an open-source project with a 10-external-contributors launch goal: who do we want as contributors, and what stack lets a stranger ship a useful dashboard PR in one evening that one maintainer can review in minutes?

## Decision

React 18 + TypeScript (strict, no `any`) + Vite + TanStack Query + Tailwind CSS + shadcn/ui, built as a SPA and served by the dashboard's Go binary via `go:embed` — no Node server in the product.

- **React + TS** is the largest pool of drive-by OSS frontend contributors; choosing Svelte/Solid would filter that pool for taste, not product benefit.
- **TanStack Query** removes the bespoke fetch/cache/invalidation state machines that dominate dashboard bug reports — exactly the code one reviewer doesn't want to re-derive per PR.
- **shadcn/ui is vendored** — copied-in MIT source, no runtime dependency: zero component-library churn for Renovate, no license entanglement with AGPL distribution, and contributors patch components directly.
- **Tailwind** makes style changes reviewable as text diffs in the component file, not in a parallel CSS tree.
- **Vite** is the boring default build tool with first-class React+TS templates.

## Consequences

- A `package.json` dependency tree enters the repo in Phase 2; lockfile + Renovate + the AC-03 abuse case already account for it.
- `go:embed` keeps the deployable a single Go binary and keeps Node out of the runtime attack surface; the trade is a rebuild for any UI change (fine — UI ships with the binary anyway).
- SPA means the dashboard API is a real API from day one, which Phase 2's impersonated reads and the audit log want regardless.

## Alternatives considered

- **HTMX + Go templates** — beautifully small, but live log streaming and the multi-step wizard push toward client state anyway, and the drive-by frontend contributor pool for HTMX is a fraction of React's.
- **Next.js** — adds a Node server to a self-hosted security product: larger attack surface, second runtime to patch, for SSR no dashboard needs.
- **Svelte/Solid** — finer frameworks per taste; smaller contributor funnel, which is the criterion here.
