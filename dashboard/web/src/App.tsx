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
import { navigate } from "@/lib/router";
import { readGitHubResult, stripGitHubParams } from "@/setup/github";
import { Shell } from "@/shell/Shell";

export default function App() {
  // Pure reads in the initializers (StrictMode-safe); the URL is scrubbed in
  // the effect so a reload doesn't re-surface a stale result.
  const [ssoError, setSsoError] = useState(readSSOError);
  // The GitHub manifest callback redirects to "/?github=connected" or
  // "/?github_error=<code>"; surface it and land back on the setup wizard.
  const [githubResult, setGithubResult] = useState(readGitHubResult);
  useEffect(() => {
    stripSSOErrorParam();
    if (readGitHubResult() !== null) {
      stripGitHubParams();
      navigate("/setup");
    }
  }, []);

  const banner =
    ssoError || githubResult ? (
      <div className="flex w-full flex-col gap-2">
        {ssoError && (
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
        )}
        {githubResult && (
          <Alert
            variant={githubResult.ok ? "default" : "destructive"}
            className="w-full"
          >
            <AlertTitle>
              {githubResult.ok ? "GitHub App connected" : "GitHub connection failed"}
            </AlertTitle>
            <AlertDescription>
              {githubResult.ok
                ? "Push webhooks now reach this install once the App is installed on your repositories."
                : githubResult.message}
              <Button
                variant="ghost"
                size="sm"
                onClick={() => {
                  setGithubResult(null);
                }}
              >
                Dismiss
              </Button>
            </AlertDescription>
          </Alert>
        )}
      </div>
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
