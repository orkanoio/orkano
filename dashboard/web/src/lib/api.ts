// Minimal typed client for the dashboard's own Go API. Grows with the auth and
// App/catalog screens (M2.6 sub-commits 4-5); for now it carries the one call
// the scaffold shell renders.

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

export async function fetchAuthStatus(): Promise<AuthStatus> {
  const res = await fetch("/api/auth/status", {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw new Error(`HTTP ${res.status}`);
  }
  return (await res.json()) as AuthStatus;
}
