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
    case "name_in_use":
      return err.existingKind
        ? `That name is already used by an existing ${err.existingKind} resource — choose another.`
        : "That name is already used by another Orkano resource — choose another.";
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
    case "name_conflict":
      return "That name would collide with an existing Secret (a database's connection Secret or a vault sync) — pick another.";
    case "secrets_vault_not_installed":
      return "The External Secrets Operator is not installed — re-run the installer with --secrets-vault to add it.";
    case "credentials_name_taken":
      return "Something else already owns the Secret this store's credentials would use — pick a different store name.";
    case "vault_server_must_be_https":
      return "The Vault server URL must be https:// — the store credential travels over that connection.";
    case "invalid_vault_path":
      return "Enter the Vault secrets-engine mount path (for example: secret).";
    case "invalid_vault_version":
      return "The KV engine version must be v1 or v2.";
    case "missing_token":
      return "A Vault token is required to connect the store.";
    case "reserved_name":
      return "Names ending in -credentials or -env are reserved — pick another.";
    case "unknown_store":
      return "That secret store does not exist — connect it first.";
    case "invalid_refresh_interval":
      return "Enter a refresh interval like 1h, 30m, or 24h.";
    case "invalid_keys":
      return "Each key needs a valid variable name (letters, digits, underscores; unique) and a vault path.";
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
