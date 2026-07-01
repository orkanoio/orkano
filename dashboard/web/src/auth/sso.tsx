import { Button } from "@/components/ui/button";
import { oidcLoginPath } from "@/lib/api";

import { ssoErrorMessage } from "./messages";

// readSSOError returns the human message for the ?sso_error=<code> query
// param the OIDC callback redirects with on failure (server/oidc.go). A pure
// read — the param is stripped separately (stripSSOErrorParam) so React
// StrictMode's double-invoked state initializers stay safe.
export function readSSOError(): string | null {
  const code = new URLSearchParams(window.location.search).get("sso_error");
  return code ? ssoErrorMessage(code) : null;
}

// stripSSOErrorParam scrubs sso_error from the address bar so a reload or a
// copied URL does not re-surface a stale failure.
export function stripSSOErrorParam(): void {
  const url = new URL(window.location.href);
  if (!url.searchParams.has("sso_error")) {
    return;
  }
  url.searchParams.delete("sso_error");
  window.history.replaceState(null, "", url);
}

// SSOSignIn renders the "or — Sign in with SSO" block under a credential form
// when the server reports OIDC configured. A real navigation (anchor), not a
// fetch: the endpoint replies with a redirect out to the IdP.
export function SSOSignIn({ enabled }: { enabled: boolean }) {
  if (!enabled) {
    return null;
  }
  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-3" aria-hidden="true">
        <div className="bg-border h-px flex-1" />
        <span className="text-muted-foreground text-xs">or</span>
        <div className="bg-border h-px flex-1" />
      </div>
      <Button asChild variant="outline" className="w-full">
        <a href={oidcLoginPath}>Sign in with SSO</a>
      </Button>
    </div>
  );
}
