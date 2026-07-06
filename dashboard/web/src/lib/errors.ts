import { ApiError } from "@/lib/api";

// isStepUpRequired reports whether a mutation was refused because the session
// needs a fresh second factor (RequireStepUp, 403 step_up_required) — the
// screens open StepUpForm and retry instead of showing an inline error.
export function isStepUpRequired(err: unknown): boolean {
  return err instanceof ApiError && err.code === "step_up_required";
}

// apiErrorMessage renders a failed App/catalog API call for humans. The codes
// mirror dashboard/internal/server (writeK8sError + the handler-local codes);
// anything unmapped degrades by HTTP status. The server deliberately returns
// only stable codes — apiserver validation detail stays in its logs — so the
// forms mirror the constraints client-side to say anything more specific.
export function apiErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return "Could not reach the API — check the connection and try again.";
  }
  switch (err.code) {
    case "not_found":
      return "Not found — it may have just been deleted.";
    case "already_exists":
      return "That name is already taken.";
    case "conflict":
      return "Someone else changed this at the same time — reload and retry.";
    case "invalid":
      return "The cluster rejected this change as invalid.";
    case "invalid_name":
      return "Names must be lowercase letters, digits, and hyphens.";
    case "invalid_request":
    case "bad_request":
      return "The request was malformed — check the field values.";
    case "invalid_env":
      return "Variable names must start with a letter or underscore and use only letters, digits, and underscores.";
    case "env_limit_exceeded":
      return "An app can have at most 64 environment variables.";
    case "app_name_too_long":
      return "The app name is too long to derive its env Secret name.";
    case "storage_shrink_forbidden":
      return "Storage can only grow — enter a size at least as large as the current one.";
    case "unauthorized":
      return "The session expired — sign in again.";
    case "forbidden":
      return "The dashboard is not permitted to do that — check its RBAC.";
    case "unavailable":
      return "The cluster API is unavailable — try again shortly.";
    case "cluster_not_ready":
      // The server's lazy RESTMapper re-checks discovery on every call, so once
      // the installer establishes the CRDs this heals on the next poll — waiting
      // is the primary remediation, re-running the (idempotent) installer the
      // escalation.
      return "Orkano is still finishing its install — the cluster is missing Orkano's CRDs. This usually resolves itself within a minute; if it persists, re-run the installer.";
  }
  if (err.status === 429) {
    return "Too many attempts — wait a minute and try again.";
  }
  if (err.status >= 500) {
    return "The server hit an internal error — try again.";
  }
  return `Request failed (${err.code}).`;
}
