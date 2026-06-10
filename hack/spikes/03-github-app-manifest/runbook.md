# Spike 3: GitHub App manifest flow

Proves the mechanics Phase 2's onboarding wizard will embed: serve a manifest
form, let GitHub create the app, exchange the temporary code for credentials.

## Prerequisites

- A GitHub test org you own. That's it — no tokens, no env vars; the
  manifest conversion endpoint is unauthenticated.
- (No `--org` also works: the app is created under your personal account.)

## Run it

```sh
cd hack/spikes/03-github-app-manifest
GOWORK=off go run . --org <your-test-org>
```

`GOWORK=off` is required: the repo's `go.work` does not (and should not)
list this throwaway module.

1. Your browser opens `http://localhost:8765` (a URL is printed if it
   doesn't). The page auto-submits the manifest to GitHub.
2. GitHub shows its app-creation screen: name `orkano-dev-test`,
   webhook URL `https://example.invalid/webhook`, permissions
   **Contents: Read-only** and **Metadata: Read-only**, subscribed event
   **push**. App names are unique across all of GitHub — if the name is
   taken, edit it right on that screen (or rerun with `--app-name`).
3. Click **Create GitHub App for &lt;org&gt;**. GitHub redirects to
   `http://localhost:8765/callback?code=…&state=…`; the helper verifies the
   state, exchanges the code (single use, expires in ~1 hour), writes the
   credentials, prints a summary, and exits.

## What lands in out/

`out/` is created at runtime (0700) and gitignored via `hack/spikes/*/out/`.

- `out/app.json` (0600) — the full conversion response minus the private
  key: `id`, `slug`, `client_id`, `client_secret`, `webhook_secret`,
  `html_url`, `owner`, plus whatever else GitHub returns.
- `out/app.private-key.pem` (0600) — the app's RSA private key.

## Where each credential goes in the real product

| Credential | Destination |
| --- | --- |
| `webhook_secret` | webhook receiver's HMAC key (verifies `X-Hub-Signature-256`) |
| `pem` | Kubernetes Secret read by the operator only — NEVER the DB (INV-07) |
| app `id`, `client_id` | operator config (non-secret identifiers) |
| `client_secret` | unused unless we add user-to-server OAuth; Kubernetes Secret if so |

## What to capture for Phase 2

- The full field list of the conversion response — open `out/app.json` and
  record every key, so the wizard's types are complete.
- Redirect parameter shapes: exactly which query params GitHub sends to
  `redirect_url` (`code`, `state` — confirm nothing else) and what the
  GitHub error path looks like if the user cancels.
- State handling: random per run, carried as a query param on the form's
  action URL, round-tripped by GitHub, verified before the exchange.
- Creating the app does NOT install it. Installation
  (`https://github.com/apps/<slug>/installations/new`) is a separate flow
  with its own redirect params (`installation_id`, `setup_action`) — note
  what it sends; the wizard needs both halves.

## Cleanup

- Delete the test app: GitHub → org **Settings → Developer settings →
  GitHub Apps → orkano-dev-test → Advanced → Delete GitHub App** (personal
  account: same path under your own Settings).
- `rm -rf out/` — the credentials are dead once the app is deleted, but
  don't leave keys lying around.

## What the wizard does differently

- Pre-fills the manifest's webhook URL with the install's real receiver
  endpoint, not the `example.invalid` placeholder.
- Uses the dashboard's own callback URL as `redirect_url`/`callback_urls`
  instead of localhost.
- Stores the credentials straight into Kubernetes Secrets at exchange time —
  nothing on local disk, and the pem never touches the DB.
