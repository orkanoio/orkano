import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type ReactNode } from "react";

import { BootstrapFlow } from "@/auth/BootstrapFlow";
import { LoginFlow } from "@/auth/LoginFlow";
import { readSSOError, stripSSOErrorParam } from "@/auth/sso";
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
  type AuthStatus,
} from "@/lib/api";
import { Shell } from "@/shell/Shell";

export default function App() {
  // A pure read in the initializer (StrictMode-safe); the URL is scrubbed in
  // the effect so a reload doesn't re-surface a stale SSO failure.
  const [ssoError, setSsoError] = useState(readSSOError);
  useEffect(() => {
    stripSSOErrorParam();
  }, []);

  const banner = ssoError ? (
    <Alert variant="destructive" className="w-full">
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
  ) : null;

  return <AuthGate banner={banner} />;
}

// AuthGate branches the whole SPA on /api/auth/status: bootstrap wizard,
// two-step login, or the signed-in shell.
function AuthGate({ banner }: { banner: ReactNode }) {
  const queryClient = useQueryClient();
  const { data, error, isPending, refetch } = useQuery({
    queryKey: authStatusKey,
    queryFn: fetchAuthStatus,
  });

  if (isPending) {
    return (
      <CenteredPage banner={banner}>
        <p className="text-muted-foreground text-sm">Loading…</p>
      </CenteredPage>
    );
  }
  if (error) {
    return (
      <CenteredPage banner={banner}>
        <AuthCard title="Orkano" description="Cannot reach the Orkano API.">
          <Button
            variant="outline"
            className="w-full"
            onClick={() => void refetch()}
          >
            Retry
          </Button>
        </AuthCard>
      </CenteredPage>
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
        <CenteredPage banner={banner}>
          <AuthCard
            title="Set up Orkano"
            description="Redeem the install token to create the admin account. Two-factor authentication is required."
          >
            <BootstrapFlow
              oidcEnabled={data.oidcEnabled}
              onAuthenticated={onAuthenticated}
            />
          </AuthCard>
        </CenteredPage>
      );
    case "needs_login":
      return (
        <CenteredPage banner={banner}>
          <AuthCard
            title="Sign in to Orkano"
            description="Open-source, self-hosted PaaS that makes Kubernetes as easy as Heroku."
          >
            <LoginFlow
              oidcEnabled={data.oidcEnabled}
              onAuthenticated={onAuthenticated}
            />
          </AuthCard>
        </CenteredPage>
      );
    case "authenticated":
      return <Shell status={data} banner={banner} />;
  }
}

// CenteredPage is the pre-auth layout: one card in the middle of the screen.
function CenteredPage({
  banner,
  children,
}: {
  banner: ReactNode;
  children: ReactNode;
}) {
  return (
    <main className="mx-auto flex min-h-svh w-full max-w-sm flex-col items-center justify-center gap-4 p-6">
      {banner}
      {children}
    </main>
  );
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
    <Card className="w-full">
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
