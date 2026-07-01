import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type ReactNode } from "react";

import { StepUpForm } from "@/auth/StepUpForm";
import { AppDetail } from "@/apps/AppDetail";
import { AppForm } from "@/apps/AppForm";
import { AppList } from "@/apps/AppList";
import { PostgresDetail } from "@/catalog/PostgresDetail";
import { PostgresForm } from "@/catalog/PostgresForm";
import { PostgresList } from "@/catalog/PostgresList";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { authStatusKey, logout, type AuthStatus } from "@/lib/api";
import { Link, routeSegments, useRoute } from "@/lib/router";
import { cn } from "@/lib/utils";

import { SessionContext } from "./session";

// stepUpFreshnessMs mirrors the server's stepUpFreshness (session.go): the
// "unlocked" banner must not outlive the window RequireStepUp enforces.
const stepUpFreshnessMs = 5 * 60 * 1000;

// Shell is the signed-in application frame: brand, Apps/Databases navigation,
// and the session controls (proactive step-up re-auth, sign out) that sub-4's
// placeholder carried — plus the hash-routed App/catalog screens.
export function Shell({
  status,
  banner,
}: {
  status: Extract<AuthStatus, { state: "authenticated" }>;
  banner?: ReactNode;
}) {
  const route = useRoute();

  return (
    <SessionContext.Provider
      value={{ username: status.username, oidc: status.oidc }}
    >
      <div className="min-h-svh">
        <header className="bg-card border-b">
          <div className="mx-auto flex w-full max-w-5xl flex-wrap items-center gap-4 px-6 py-3">
            <Link to="/apps" className="text-primary text-lg font-semibold">
              Orkano
            </Link>
            <nav className="flex gap-1">
              <NavLink to="/apps" active={isSection(route, "apps")}>
                Apps
              </NavLink>
              <NavLink to="/databases" active={isSection(route, "databases")}>
                Databases
              </NavLink>
            </nav>
            <SessionControls username={status.username} oidc={status.oidc} />
          </div>
        </header>
        <main className="mx-auto flex w-full max-w-5xl flex-col gap-6 p-6">
          {banner}
          {routeContent(routeSegments(route))}
        </main>
      </div>
    </SessionContext.Provider>
  );
}

function isSection(route: string, section: string): boolean {
  const head = routeSegments(route)[0] ?? "apps";
  return head === section;
}

function NavLink({
  to,
  active,
  children,
}: {
  to: string;
  active: boolean;
  children: ReactNode;
}) {
  return (
    <Link
      to={to}
      aria-current={active ? "page" : undefined}
      className={cn(
        "rounded-md px-3 py-1.5 text-sm font-medium",
        active
          ? "bg-secondary text-foreground"
          : "text-muted-foreground hover:text-foreground",
      )}
    >
      {children}
    </Link>
  );
}

function routeContent(segments: string[]): ReactNode {
  const [head, second, third] = segments;
  // The detail/edit screens are keyed by the object name: navigating between
  // two objects of the same kind must REMOUNT the screen, or form state
  // initialized from the first object survives a cache-hit prop swap (no
  // isPending gap) and a save would write object A's fields onto object B.
  if (head === undefined || head === "apps") {
    if (second === "new") {
      return <AppForm />;
    }
    if (second !== undefined && third === "edit") {
      return <AppForm key={second} edit={second} />;
    }
    if (second !== undefined) {
      return <AppDetail key={second} name={second} />;
    }
    return <AppList />;
  }
  if (head === "databases") {
    if (second === "new") {
      return <PostgresForm />;
    }
    if (second !== undefined) {
      return <PostgresDetail key={second} name={second} />;
    }
    return <PostgresList />;
  }
  return (
    <p className="text-muted-foreground text-sm">
      Nothing here —{" "}
      <Link to="/apps" className="text-primary hover:underline">
        back to apps
      </Link>
      .
    </p>
  );
}

// SessionControls keeps sub-4's session surface alive: proactive re-auth
// (unlock destructive actions for the next 5 minutes) and sign out.
function SessionControls({
  username,
  oidc,
}: {
  username: string;
  oidc: boolean;
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
    <div className="ml-auto flex items-center gap-2">
      <span className="text-muted-foreground text-sm">
        <span className="text-foreground font-medium">{username}</span>
        {oidc ? " via SSO" : ""}
      </span>
      <Button
        variant="outline"
        size="sm"
        onClick={() => {
          setReauthOpen(true);
        }}
      >
        Re-authenticate
      </Button>
      <Button
        variant="ghost"
        size="sm"
        disabled={signOut.isPending}
        onClick={() => {
          signOut.mutate();
        }}
      >
        {signOut.isPending ? "Signing out…" : "Sign out"}
      </Button>
      {reauthOpen && (
        <div
          role="dialog"
          aria-modal="true"
          aria-label="Re-authenticate"
          className="fixed inset-x-0 top-16 z-10 flex justify-center px-6"
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              setReauthOpen(false);
              setReauthDone(false);
            }
          }}
        >
          <Card className="w-full max-w-sm shadow-lg">
            <CardContent className="flex flex-col gap-3">
              {reauthDone ? (
                <>
                  <Alert>
                    <AlertDescription>
                      Identity confirmed — destructive actions are unlocked for
                      the next 5 minutes.
                    </AlertDescription>
                  </Alert>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => {
                      setReauthOpen(false);
                      setReauthDone(false);
                    }}
                  >
                    Close
                  </Button>
                </>
              ) : (
                <>
                  <StepUpForm
                    oidc={oidc}
                    onDone={() => {
                      setReauthDone(true);
                    }}
                  />
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => {
                      setReauthOpen(false);
                    }}
                  >
                    Cancel
                  </Button>
                </>
              )}
            </CardContent>
          </Card>
        </div>
      )}
    </div>
  );
}
