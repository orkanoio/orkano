import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Database,
  KeyRound,
  LayoutGrid,
  SlidersHorizontal,
  type LucideIcon,
} from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";

import { StepUpForm } from "@/auth/StepUpForm";
import { AppDetail } from "@/apps/AppDetail";
import { AppForm } from "@/apps/AppForm";
import { AppList } from "@/apps/AppList";
import { PostgresDetail } from "@/catalog/PostgresDetail";
import { PostgresForm } from "@/catalog/PostgresForm";
import { PostgresList } from "@/catalog/PostgresList";
import { StoreForm } from "@/vault/StoreForm";
import { SyncForm } from "@/vault/SyncForm";
import { VaultPage } from "@/vault/VaultPage";
import { Logo } from "@/components/Logo";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { authStatusKey, logout, type AuthStatus } from "@/lib/api";
import { Link, routeSegments, useRoute } from "@/lib/router";
import { cn } from "@/lib/utils";
import { SetupWizard } from "@/setup/SetupWizard";

import { SessionContext } from "./session";

// stepUpFreshnessMs mirrors the server's stepUpFreshness (session.go): the
// "unlocked" banner must not outlive the window RequireStepUp enforces.
const stepUpFreshnessMs = 5 * 60 * 1000;

// Shell is the signed-in application frame, laid out landing-style: a dark
// sidebar rail (brand, Apps/Databases/Vault/Setup navigation, session
// controls) beside the hash-routed content pane.
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
      <div className="flex min-h-svh flex-col md:flex-row">
        <aside className="bg-sidebar flex w-full shrink-0 flex-col border-b md:sticky md:top-0 md:h-svh md:w-60 md:border-r md:border-b-0">
          <Link
            to="/apps"
            className="flex h-[62px] shrink-0 items-center gap-2.5 border-b px-5"
          >
            <Logo size={22} />
            <span className="font-display text-[15px] font-semibold tracking-tight text-white">
              orkano
            </span>
          </Link>
          <nav className="flex gap-1 overflow-x-auto p-2 md:flex-col md:p-3">
            <NavLink
              to="/apps"
              active={isSection(route, "apps")}
              icon={LayoutGrid}
            >
              Apps
            </NavLink>
            <NavLink
              to="/databases"
              active={isSection(route, "databases")}
              icon={Database}
            >
              Databases
            </NavLink>
            <NavLink
              to="/vault"
              active={isSection(route, "vault")}
              icon={KeyRound}
            >
              Vault
            </NavLink>
            <NavLink
              to="/setup"
              active={isSection(route, "setup")}
              icon={SlidersHorizontal}
            >
              Setup
            </NavLink>
          </nav>
          <SessionControls username={status.username} oidc={status.oidc} />
        </aside>
        <div className="min-w-0 flex-1">
          <main className="mx-auto flex w-full max-w-7xl flex-col gap-6 p-4 sm:p-6 lg:p-8">
            {banner}
            {routeContent(routeSegments(route))}
          </main>
        </div>
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
  icon: Icon,
  children,
}: {
  to: string;
  active: boolean;
  icon: LucideIcon;
  children: ReactNode;
}) {
  return (
    <Link
      to={to}
      aria-current={active ? "page" : undefined}
      className={cn(
        "flex shrink-0 items-center gap-2.5 rounded-md border px-3 py-2 font-mono text-[12.5px] transition-colors",
        active
          ? "border-primary/25 bg-primary/8 text-primary"
          : "border-transparent text-muted-foreground hover:text-foreground",
      )}
    >
      <Icon className="size-4" aria-hidden="true" />
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
  if (head === "vault") {
    if (second === "connect") {
      // Rotation is keyed by the store name — same remount rule as the other
      // edit screens.
      return third !== undefined ? (
        <StoreForm key={third} edit={third} />
      ) : (
        <StoreForm />
      );
    }
    if (second === "sync") {
      return <SyncForm />;
    }
    return <VaultPage />;
  }
  if (head === "setup") {
    return <SetupWizard />;
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

// SessionControls is the sidebar footer: the signed-in identity, proactive
// re-auth (unlock destructive actions for the next 5 minutes), and sign out.
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
    <div className="flex flex-wrap items-center gap-2 border-t p-3 md:mt-auto md:flex-col md:items-stretch md:p-4">
      <span className="text-muted-foreground truncate font-mono text-[11px]">
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
                  <Alert variant="success">
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
