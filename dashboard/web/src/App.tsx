import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type ReactNode } from "react";

import { BootstrapFlow } from "@/auth/BootstrapFlow";
import { LoginFlow } from "@/auth/LoginFlow";
import { readSSOError, stripSSOErrorParam } from "@/auth/sso";
import { StepUpForm } from "@/auth/StepUpForm";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  authStatusKey,
  fetchAuthStatus,
  logout,
  type AuthStatus,
} from "@/lib/api";

export default function App() {
  // A pure read in the initializer (StrictMode-safe); the URL is scrubbed in
  // the effect so a reload doesn't re-surface a stale SSO failure.
  const [ssoError, setSsoError] = useState(readSSOError);
  useEffect(() => {
    stripSSOErrorParam();
  }, []);

  return (
    <main className="flex min-h-svh flex-col items-center justify-center gap-4 p-6">
      {ssoError && (
        <Alert variant="destructive" className="w-full max-w-sm">
          <AlertTitle>Single sign-on error</AlertTitle>
          <AlertDescription>
            {ssoError}
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                setSsoError(null);
              }}
            >
              Dismiss
            </Button>
          </AlertDescription>
        </Alert>
      )}
      <AuthGate />
    </main>
  );
}

// AuthGate branches the whole SPA on /api/auth/status: bootstrap wizard,
// two-step login, or the signed-in shell.
function AuthGate() {
  const queryClient = useQueryClient();
  const { data, error, isPending, refetch } = useQuery({
    queryKey: authStatusKey,
    queryFn: fetchAuthStatus,
  });

  if (isPending) {
    return <p className="text-muted-foreground text-sm">Loading…</p>;
  }
  if (error) {
    return (
      <AuthCard title="Orkano" description="Cannot reach the Orkano API.">
        <Button
          variant="outline"
          className="w-full"
          onClick={() => void refetch()}
        >
          Retry
        </Button>
      </AuthCard>
    );
  }

  // Settle the gate instantly on a successful sign-in (the session cookie is
  // already set), then reconcile against the server in the background.
  const onAuthenticated = (username: string) => {
    const settled: AuthStatus = {
      state: "authenticated",
      username,
      oidc: false,
      oidcEnabled: data.oidcEnabled,
    };
    queryClient.setQueryData(authStatusKey, settled);
    void queryClient.invalidateQueries({ queryKey: authStatusKey });
  };

  switch (data.state) {
    case "needs_bootstrap":
      return (
        <AuthCard
          title="Set up Orkano"
          description="Redeem the install token to create the admin account. Two-factor authentication is required."
        >
          <BootstrapFlow
            oidcEnabled={data.oidcEnabled}
            onAuthenticated={onAuthenticated}
          />
        </AuthCard>
      );
    case "needs_login":
      return (
        <AuthCard
          title="Sign in to Orkano"
          description="Open-source, self-hosted PaaS that makes Kubernetes as easy as Heroku."
        >
          <LoginFlow
            oidcEnabled={data.oidcEnabled}
            onAuthenticated={onAuthenticated}
          />
        </AuthCard>
      );
    case "authenticated":
      return <SignedInShell status={data} />;
  }
}

function AuthCard({
  title,
  description,
  children,
}: {
  title: string;
  description: string;
  children: ReactNode;
}) {
  return (
    <Card className="w-full max-w-sm">
      <CardHeader>
        {/* CardTitle is a div by design; the page-level title still needs
            heading semantics for screen-reader navigation. */}
        <CardTitle
          role="heading"
          aria-level={1}
          className="text-primary text-2xl"
        >
          {title}
        </CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  );
}

// stepUpFreshnessMs mirrors the server's stepUpFreshness (session.go): the
// "unlocked" banner must not outlive the window RequireStepUp enforces.
const stepUpFreshnessMs = 5 * 60 * 1000;

// SignedInShell is the placeholder authenticated screen — the App/catalog
// screens replace it in M2.6 sub-commit 5. It keeps the session controls
// (step-up re-auth, sign out) live so the whole auth surface is exercisable.
function SignedInShell({
  status,
}: {
  status: Extract<AuthStatus, { state: "authenticated" }>;
}) {
  const queryClient = useQueryClient();
  const [reauthOpen, setReauthOpen] = useState(false);
  const [reauthDone, setReauthDone] = useState(false);

  useEffect(() => {
    if (!reauthDone) {
      return;
    }
    const timer = setTimeout(() => {
      setReauthDone(false);
      setReauthOpen(false);
    }, stepUpFreshnessMs);
    return () => {
      clearTimeout(timer);
    };
  }, [reauthDone]);

  const signOut = useMutation({
    mutationFn: logout,
    // Even a failed logout (expired session) should re-check the gate.
    onSettled: () =>
      queryClient.invalidateQueries({ queryKey: authStatusKey }),
  });

  return (
    <Card className="w-full max-w-sm">
      <CardHeader>
        <CardTitle
          role="heading"
          aria-level={1}
          className="text-primary text-2xl"
        >
          Orkano
        </CardTitle>
        <CardDescription>
          Signed in as{" "}
          <span className="text-foreground font-medium">{status.username}</span>
          {status.oidc ? " via SSO" : ""}. The App screens land in the next
          milestone step.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {reauthOpen ? (
          reauthDone ? (
            <Alert>
              <AlertDescription>
                Identity confirmed — destructive actions are unlocked for the
                next 5 minutes.
              </AlertDescription>
            </Alert>
          ) : (
            <StepUpForm
              oidc={status.oidc}
              onDone={() => {
                setReauthDone(true);
              }}
            />
          )
        ) : (
          <Button
            variant="outline"
            onClick={() => {
              setReauthOpen(true);
            }}
          >
            Re-authenticate
          </Button>
        )}
        <Button
          variant="ghost"
          disabled={signOut.isPending}
          onClick={() => {
            signOut.mutate();
          }}
        >
          {signOut.isPending ? "Signing out…" : "Sign out"}
        </Button>
      </CardContent>
    </Card>
  );
}
