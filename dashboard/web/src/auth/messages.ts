import { ApiError } from "@/lib/api";

// authErrorMessage renders a failed auth call for humans. The codes mirror
// dashboard/internal/server (auth.go); anything unmapped degrades by HTTP
// status so a new server code still yields an honest generic message.
export function authErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return "Could not reach the API — check the connection and try again.";
  }
  switch (err.code) {
    case "invalid_token":
      return "That install token is not valid.";
    case "already_bootstrapped":
      return "Setup is already complete — sign in instead.";
    case "invalid_username":
      return "Enter a username of at most 254 characters.";
    case "weak_password":
      return "Passwords must be at least 12 characters and at most 72 bytes.";
    case "invalid_credentials":
      return "Invalid credentials.";
    case "account_locked":
      return "Account locked after too many failed attempts — try again in 15 minutes.";
    case "invalid_code":
      return "That code is not valid — check your authenticator app.";
    case "no_challenge":
    case "unauthorized":
      return "This step expired — start over.";
    case "oidc_stepup_required":
      return "This session re-authenticates through the identity provider.";
  }
  if (err.status === 429) {
    return "Too many attempts — wait a minute and try again.";
  }
  if (err.status >= 500) {
    return "The server hit an internal error — try again.";
  }
  return `Request failed (${err.code}).`;
}

// isExpiredChallenge reports whether a second-factor call failed because the
// short-lived challenge cookie is gone (expired, cleared, or spent) — the
// flow should restart from its first step rather than show an inline error.
export function isExpiredChallenge(err: unknown): boolean {
  return (
    err instanceof ApiError &&
    (err.code === "no_challenge" || err.code === "unauthorized")
  );
}

// ssoErrorMessages maps the fixed ?sso_error= codes the OIDC callback
// redirects with (server/oidc.go) to human messages.
const ssoErrorMessages: Record<string, string> = {
  disabled: "SSO sign-in is not configured on this install.",
  no_flow: "The SSO sign-in attempt expired — try again.",
  state_mismatch: "The SSO sign-in attempt could not be verified — try again.",
  exchange_failed: "The identity provider sign-in could not be completed.",
  not_allowed: "That identity is not allowed to access this dashboard.",
  internal_error: "SSO sign-in hit an internal error — try again.",
  idp_error: "The identity provider reported an error.",
  no_session: "Sign in before re-authenticating.",
  not_oidc: "This account re-authenticates with its password and code, not SSO.",
};

export function ssoErrorMessage(code: string): string {
  return ssoErrorMessages[code] ?? "SSO sign-in failed.";
}
