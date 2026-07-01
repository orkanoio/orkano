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

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: "application/json" } });
  if (!res.ok) {
    throw await parseError(res);
  }
  return (await res.json()) as T;
}

// post sends a JSON body (or none) and returns the raw Response so callers of
// 204-No-Content endpoints skip the body parse.
async function post(path: string, body?: unknown): Promise<Response> {
  const res = await fetch(path, {
    method: "POST",
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

async function postJSON<T>(path: string, body?: unknown): Promise<T> {
  return (await (await post(path, body)).json()) as T;
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
