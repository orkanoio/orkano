// Typed client for the dashboard's own Go API (dashboard/internal/server).
// Request/response shapes mirror the handlers exactly; every non-2xx response
// becomes an ApiError carrying the server's machine-readable error code so
// screens can branch on codes instead of string-matching messages.

// Query key for the auth status — shared by the gate and by the flows that
// settle it after a successful sign-in.
export const authStatusKey = ["auth", "status"] as const;

// Discriminated on state, mirroring handleAuthStatus exactly: username/oidc
// are sent only on the authenticated branch, so narrowing on state gives the
// auth screens compiler-checked access instead of optional-everywhere fields.
export type AuthStatus =
  | { state: "needs_bootstrap"; oidcEnabled: boolean }
  | { state: "needs_login"; oidcEnabled: boolean }
  | {
      state: "authenticated";
      oidcEnabled: boolean;
      username: string;
      oidc: boolean;
    };

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string) {
    super(`${status.toString()} ${code}`);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

// parseError builds the ApiError for a non-2xx response: the server's
// {"error": code} body when present, else a synthetic status-only code (a
// proxy error page or an empty body must not crash error handling).
async function parseError(res: Response): Promise<ApiError> {
  let code = "";
  try {
    const body: unknown = await res.json();
    if (
      typeof body === "object" &&
      body !== null &&
      "error" in body &&
      typeof body.error === "string"
    ) {
      code = body.error;
    }
  } catch {
    // Non-JSON error body — fall through to the status-only code.
  }
  return new ApiError(res.status, code || `http_${res.status.toString()}`);
}

// errorFromResponse is parseError for callers outside this module that drive
// fetch themselves (the SSE log stream).
export function errorFromResponse(res: Response): Promise<ApiError> {
  return parseError(res);
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: "application/json" } });
  if (!res.ok) {
    throw await parseError(res);
  }
  return (await res.json()) as T;
}

// send issues a mutating request with an optional JSON body and returns the raw
// Response so callers of 204-No-Content endpoints skip the body parse.
async function send(
  method: "POST" | "PUT" | "DELETE",
  path: string,
  body?: unknown,
): Promise<Response> {
  const res = await fetch(path, {
    method,
    headers:
      body === undefined
        ? { Accept: "application/json" }
        : { Accept: "application/json", "Content-Type": "application/json" },
    body: body === undefined ? null : JSON.stringify(body),
  });
  if (!res.ok) {
    throw await parseError(res);
  }
  return res;
}

async function post(path: string, body?: unknown): Promise<Response> {
  return send("POST", path, body);
}

async function postJSON<T>(path: string, body?: unknown): Promise<T> {
  return (await (await post(path, body)).json()) as T;
}

async function putJSON<T>(path: string, body: unknown): Promise<T> {
  return (await (await send("PUT", path, body)).json()) as T;
}

export function fetchAuthStatus(): Promise<AuthStatus> {
  return getJSON("/api/auth/status");
}

export interface RedeemRequest {
  token: string;
  username: string;
  password: string;
}

// RedeemResponse is shown exactly once: the server never returns the plain
// recovery codes or the otpauth URL again.
export interface RedeemResponse {
  otpauthUrl: string;
  recoveryCodes: string[];
}

export function redeemInstallToken(req: RedeemRequest): Promise<RedeemResponse> {
  return postJSON("/api/auth/redeem", req);
}

export interface AuthenticatedResult {
  state: "authenticated";
  username: string;
}

export function confirmTotp(code: string): Promise<AuthenticatedResult> {
  return postJSON("/api/auth/totp/confirm", { code });
}

export function login(
  username: string,
  password: string,
): Promise<{ state: "totp_required" }> {
  return postJSON("/api/auth/login", { username, password });
}

// The second factor is either a live authenticator code or a single-use
// recovery code — exactly one, mirroring handleLoginTOTP's precedence.
export type SecondFactor = { code: string } | { recoveryCode: string };

export function loginTotp(second: SecondFactor): Promise<AuthenticatedResult> {
  return postJSON("/api/auth/login/totp", second);
}

export async function logout(): Promise<void> {
  await post("/api/auth/logout");
}

export async function stepUp(password: string, code: string): Promise<void> {
  await post("/api/auth/stepup", { password, code });
}

// Browser-navigation entry points, not fetch targets: the OIDC endpoints
// reply with redirects (out to the IdP, back to "/"), so the SPA links to
// them with real anchors.
export const oidcLoginPath = "/api/auth/oidc/login";
export const oidcStepUpPath = "/api/auth/oidc/login?stepup=1";

// ---------------------------------------------------------------------------
// App/catalog API (M2.4 server surface). The spec/status types mirror
// api/v1alpha1's json tags verbatim — the server DTOs pass the CRD spec and
// status through unchanged, so these are the frozen public shapes.

export interface SecretKeyRef {
  name: string;
  key: string;
}

// EnvVar carries exactly one of value or secretRef (CEL-enforced XOR).
export interface EnvVar {
  name: string;
  value?: string;
  secretRef?: SecretKeyRef;
}

export interface AppSource {
  github: { repo: string; ref?: string };
  subPath?: string;
}

export interface BuildStrategy {
  strategy: "Dockerfile" | "Static";
  dockerfile?: { path?: string };
  static?: { dir: string };
}

export interface AppSpec {
  source: AppSource;
  build: BuildStrategy;
  type?: "Web" | "Worker";
  command?: string[];
  port?: number;
  replicas?: number;
  env?: EnvVar[];
  // Quantities (500m, 256Mi) serialize as JSON strings.
  resources?: { cpu?: string; memory?: string };
  healthCheck?: { path: string };
}

export interface Condition {
  type: string;
  status: "True" | "False" | "Unknown";
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
  observedGeneration?: number;
}

export interface AppStatus {
  observedGeneration?: number;
  conditions?: Condition[];
  image?: string;
  url?: string;
  availableReplicas?: number;
  latestBuild?: string;
}

export interface AppResponse {
  name: string;
  namespace: string;
  creationTimestamp: string | null;
  spec: AppSpec;
  status: AppStatus;
}

export interface DomainSpec {
  host: string;
  appRef: { name: string };
}

export interface DomainStatus {
  observedGeneration?: number;
  conditions?: Condition[];
}

export interface DomainResponse {
  name: string;
  namespace: string;
  creationTimestamp: string | null;
  spec: DomainSpec;
  status: DomainStatus;
}

export interface PostgresSpec {
  version?: string;
  storageSize?: string;
}

export interface PostgresStatus {
  observedGeneration?: number;
  conditions?: Condition[];
  secretName?: string;
}

export interface PostgresResponse {
  name: string;
  namespace: string;
  creationTimestamp: string | null;
  spec: PostgresSpec;
  status: PostgresStatus;
}

export interface DeployRow {
  occurredAt: string;
  buildName?: string;
  image?: string;
  status: string;
}

// Query keys, hierarchical so invalidating ["apps"] also drops every detail.
export const appsKey = ["apps"] as const;
export const appKey = (name: string) => ["apps", name] as const;
export const appDeploysKey = (name: string) =>
  ["apps", name, "deploys"] as const;
export const domainsKey = ["domains"] as const;
export const postgresListKey = ["postgres"] as const;
export const postgresKey = (name: string) => ["postgres", name] as const;

async function listItems<T>(path: string): Promise<T[]> {
  return (await getJSON<{ items: T[] }>(path)).items;
}

export function listApps(): Promise<AppResponse[]> {
  return listItems("/api/apps");
}

export function getApp(name: string): Promise<AppResponse> {
  return getJSON(`/api/apps/${encodeURIComponent(name)}`);
}

export function createApp(name: string, spec: AppSpec): Promise<AppResponse> {
  return postJSON("/api/apps", { name, spec });
}

export function updateApp(name: string, spec: AppSpec): Promise<AppResponse> {
  return putJSON(`/api/apps/${encodeURIComponent(name)}`, { spec });
}

export async function deleteApp(name: string): Promise<void> {
  await send("DELETE", `/api/apps/${encodeURIComponent(name)}`);
}

// setAppEnv replaces the app's COMPLETE secret-backed env set (the server
// writes the <app>-env Secret blind and reconciles spec.env). Values are
// write-only: the dashboard can never read them back (ADR-0013).
export function setAppEnv(
  name: string,
  secrets: Record<string, string>,
): Promise<AppResponse> {
  return putJSON(`/api/apps/${encodeURIComponent(name)}/env`, { secrets });
}

export function listAppDeploys(name: string): Promise<DeployRow[]> {
  return listItems(`/api/apps/${encodeURIComponent(name)}/deploys`);
}

// appLogsPath builds the SSE stream URL for lib/sse.ts (not a JSON endpoint).
export function appLogsPath(
  name: string,
  opts?: { pod?: string; follow?: boolean; tail?: number },
): string {
  const params = new URLSearchParams();
  if (opts?.pod) {
    params.set("pod", opts.pod);
  }
  if (opts?.follow !== undefined) {
    params.set("follow", String(opts.follow));
  }
  if (opts?.tail !== undefined) {
    params.set("tail", opts.tail.toString());
  }
  const query = params.toString();
  return `/api/apps/${encodeURIComponent(name)}/logs${query ? `?${query}` : ""}`;
}

export function listDomains(): Promise<DomainResponse[]> {
  return listItems("/api/domains");
}

// ---------------------------------------------------------------------------
// Onboarding wizard (M2.6 setup API). The status endpoint is the wizard face
// of the shared check registry: checks arrive in dependency order, which IS
// the wizard's walk order.

export const setupStatusKey = ["setup", "status"] as const;

export interface SetupCheck {
  id: string;
  severity: "critical" | "warning" | "info";
  summary?: string;
  outcome: "pass" | "fail" | "skip" | "error" | "blocked";
  message?: string;
  blockers?: string[];
  remediation?: string;
}

export interface SetupGitHubState {
  connected: boolean;
  appSlug?: string;
  appId?: string;
  connectedAt?: string;
}

export interface SetupStatus {
  checks: SetupCheck[];
  accessMode: string;
  webhookUrlConfigured: boolean;
  // false = the OIDC redirect URL will derive from the request Host (correct
  // for the access path in use, but worth a warning before it persists).
  publicUrlConfigured: boolean;
  // The server-authoritative callback URL when ORKANO_PUBLIC_URL pins it
  // (empty = request-derived; the client shows its own origin then).
  oidcRedirectUrl: string;
  oidcEnabled: boolean;
  // true = orkano-oidc holds newer configuration than the running process
  // loaded (initial connect, or a rotation) — prompt the rollout restart.
  oidcPendingRestart: boolean;
  github: SetupGitHubState;
}

export function fetchSetupStatus(): Promise<SetupStatus> {
  return getJSON("/api/setup/status");
}

export type AccessMode = "proxy" | "tailscale" | "iap" | "public";

export async function setAccessMode(mode: AccessMode): Promise<void> {
  await post("/api/setup/access-mode", { mode });
}

// The redirect URL is deliberately absent: the server derives the only correct
// value itself and returns it, so the admin registers exactly that at the IdP.
export interface SetupOIDCRequest {
  issuer: string;
  clientId: string;
  clientSecret: string;
  allowedEmails?: string;
  allowedGroups?: string;
}

export interface SetupOIDCResponse {
  redirectUrl: string;
  restartRequired: boolean;
}

export function configureOIDC(
  req: SetupOIDCRequest,
): Promise<SetupOIDCResponse> {
  return postJSON("/api/setup/oidc", req);
}

// startGitHubManifest fetches the App manifest + the GitHub form URL; the
// wizard then form-POSTs the manifest to GitHub (a real navigation — GitHub
// renders its App-creation screen and redirects back to the callback).
export interface GitHubManifestStart {
  postUrl: string;
  manifest: string;
}

export function startGitHubManifest(opts?: {
  org?: string;
  name?: string;
}): Promise<GitHubManifestStart> {
  const params = new URLSearchParams();
  if (opts?.org) {
    params.set("org", opts.org);
  }
  if (opts?.name) {
    params.set("name", opts.name);
  }
  const query = params.toString();
  return getJSON(`/api/github/app/manifest${query ? `?${query}` : ""}`);
}

// Domain spec is immutable — there is no updateDomain; re-pointing is
// delete-and-recreate (ADR-0006).
export function createDomain(
  name: string,
  spec: DomainSpec,
): Promise<DomainResponse> {
  return postJSON("/api/domains", { name, spec });
}

export async function deleteDomain(name: string): Promise<void> {
  await send("DELETE", `/api/domains/${encodeURIComponent(name)}`);
}

export function listPostgres(): Promise<PostgresResponse[]> {
  return listItems("/api/postgres");
}

export function getPostgres(name: string): Promise<PostgresResponse> {
  return getJSON(`/api/postgres/${encodeURIComponent(name)}`);
}

export function createPostgres(
  name: string,
  spec: PostgresSpec,
): Promise<PostgresResponse> {
  return postJSON("/api/postgres", { name, spec });
}

export function updatePostgres(
  name: string,
  spec: PostgresSpec,
): Promise<PostgresResponse> {
  return putJSON(`/api/postgres/${encodeURIComponent(name)}`, { spec });
}

export async function deletePostgres(name: string): Promise<void> {
  await send("DELETE", `/api/postgres/${encodeURIComponent(name)}`);
}

// ---------------------------------------------------------------------------
// External vault (M3.1, ADR-0018): ESO SecretStore/ExternalSecret management.
// The server owns the object shapes (auth pinned to <store>-credentials,
// creationPolicy Owner); this client only ever carries the narrow connect and
// sync fields. The token is write-only — no response ever echoes it.

export interface SecretStoreItem {
  name: string;
  creationTimestamp: string | null;
  provider: string;
  server?: string;
  path?: string;
  ready: "True" | "False" | "Unknown";
  reason?: string;
  message?: string;
}

export interface SyncKey {
  secretKey: string;
  remoteKey: string;
}

export interface ExternalSecretItem {
  name: string;
  creationTimestamp: string | null;
  storeName: string;
  refreshInterval?: string;
  keys: SyncKey[];
  ready: "True" | "False" | "Unknown";
  reason?: string;
  message?: string;
  refreshTime?: string;
}

export interface SecretStoreWrite {
  vault: { server: string; path: string; version?: string };
  // Empty on an update keeps the current credential (spec-only rewire).
  token: string;
}

export const secretStoresKey = ["vault", "stores"] as const;
export const externalSecretsKey = ["vault", "syncs"] as const;

export function listSecretStores(): Promise<SecretStoreItem[]> {
  return listItems("/api/secretstores");
}

export function createSecretStore(
  name: string,
  body: SecretStoreWrite,
): Promise<SecretStoreItem> {
  return postJSON("/api/secretstores", { name, ...body });
}

export function updateSecretStore(
  name: string,
  body: SecretStoreWrite,
): Promise<SecretStoreItem> {
  return putJSON(`/api/secretstores/${encodeURIComponent(name)}`, body);
}

export async function deleteSecretStore(name: string): Promise<void> {
  await send("DELETE", `/api/secretstores/${encodeURIComponent(name)}`);
}

export function listExternalSecrets(): Promise<ExternalSecretItem[]> {
  return listItems("/api/externalsecrets");
}

export function createExternalSecret(body: {
  name: string;
  storeName: string;
  refreshInterval?: string;
  keys: SyncKey[];
}): Promise<ExternalSecretItem> {
  return postJSON("/api/externalsecrets", body);
}

export async function deleteExternalSecret(name: string): Promise<void> {
  await send("DELETE", `/api/externalsecrets/${encodeURIComponent(name)}`);
}
