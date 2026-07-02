import { ApiError } from "@/lib/api";
import { apiErrorMessage } from "@/lib/errors";

// githubErrorMessages maps the ?github_error=<code> codes the manifest-flow
// callback redirects with (server/github.go) onto human copy — the model is
// auth/messages.ts's ssoErrorMessages.
const githubErrorMessages: Record<string, string> = {
  no_flow:
    "The GitHub connection attempt expired or was started elsewhere — try again from the setup wizard.",
  state_mismatch:
    "The GitHub redirect did not match the connection attempt — try again from the setup wizard.",
  no_code: "GitHub returned no credential code — try again.",
  exchange_failed:
    "GitHub did not complete the App creation — it may have been cancelled, or the code expired.",
  write_failed:
    "The App was created on GitHub, but storing its credentials in the cluster failed — check the dashboard logs, then reconnect.",
};

export function githubErrorMessage(code: string): string {
  return (
    githubErrorMessages[code] ?? `GitHub connection failed (${code}).`
  );
}

// blockerLabels maps the known check IDs onto the short human phrases the
// blocked-outcome badge shows — a check ID is a machine identifier, not UI copy.
const blockerLabels: Record<string, string> = {
  "github.webhook-url-configured": "the webhook URL",
  "auth.admin-bootstrapped": "the admin account",
  "auth.oidc-configured": "single sign-on",
  "setup.access-mode-chosen": "the access-mode choice",
};

export function blockerLabel(id: string | undefined): string {
  return (id && blockerLabels[id]) || "a prerequisite";
}

// setupErrorMessage maps the wizard endpoints' own codes, degrading to the
// shared API copy for everything else.
export function setupErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "invalid_access_mode":
        return "Choose one of the offered access modes.";
      case "invalid_oidc_config":
        return "The identity-provider configuration is incomplete — issuer, client ID, client secret, and at least one allowlist entry are required.";
      case "oidc_discovery_failed":
        return "The issuer did not answer OIDC discovery — check the issuer URL and that the cluster can reach it.";
      case "webhook_url_not_configured":
        return "The receiver's public webhook URL is not configured — re-run orkano init with --receiver-host, or set ORKANO_WEBHOOK_URL on the dashboard Deployment.";
    }
  }
  return apiErrorMessage(err);
}
