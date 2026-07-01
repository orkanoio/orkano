import { useQuery } from "@tanstack/react-query";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { fetchAuthStatus, type AuthStatus } from "@/lib/api";

// Scaffold shell: proves the React + Tailwind + shadcn/ui + TanStack Query
// pipeline end to end (a live query against the Go API through the embed /
// dev proxy). The auth screens replace it in M2.6 sub-commit 4.
export default function App() {
  return (
    <main className="flex min-h-svh items-center justify-center p-6">
      <Card className="w-full max-w-sm">
        <CardHeader>
          {/* CardTitle is a div by design; the page-level title still needs
              heading semantics for screen-reader navigation. */}
          <CardTitle
            role="heading"
            aria-level={1}
            className="text-2xl text-primary"
          >
            Orkano
          </CardTitle>
          <CardDescription>
            Open-source, self-hosted PaaS that makes Kubernetes as easy as
            Heroku.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <ApiStatus />
        </CardContent>
      </Card>
    </main>
  );
}

function describeState(status: AuthStatus): string {
  switch (status.state) {
    case "needs_bootstrap":
      return "setup required — redeem the install token";
    case "needs_login":
      return "sign-in required";
    case "authenticated":
      return `signed in as ${status.username}`;
  }
}

function ApiStatus() {
  const { data, error, isPending } = useQuery({
    queryKey: ["auth", "status"],
    queryFn: fetchAuthStatus,
  });

  if (isPending) {
    return <p className="text-sm text-muted-foreground">Checking API…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        API unreachable: {error.message}
      </p>
    );
  }
  return (
    <p className="text-sm text-muted-foreground">
      API reachable — {describeState(data)}.
    </p>
  );
}
