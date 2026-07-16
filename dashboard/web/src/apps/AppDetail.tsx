import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Globe2,
  KeyRound,
  LayoutDashboard,
  Rocket,
  Settings,
  SlidersHorizontal,
} from "lucide-react";
import { useState } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import {
  DetailWorkspace,
  type DetailSection,
} from "@/components/DetailWorkspace";
import { StatusBadge } from "@/components/StatusBadge";
import { StepUpGate } from "@/components/StepUpGate";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  ApiError,
  appKey,
  appsKey,
  deleteApp,
  getApp,
} from "@/lib/api";
import { formatAge } from "@/lib/format";
import { Link, navigate } from "@/lib/router";

import { DeploysCard } from "./DeploysCard";
import { DomainsCard } from "./DomainsCard";
import { SecretsCard, VarsCard } from "./EnvEditor";
import { LogsCard } from "./LogsCard";

type AppSection =
  | "overview"
  | "variables"
  | "secrets"
  | "domains"
  | "deploys"
  | "settings";

const appSections: readonly DetailSection<AppSection>[] = [
  { id: "overview", label: "Overview", icon: LayoutDashboard },
  { id: "variables", label: "Variables", icon: SlidersHorizontal },
  { id: "secrets", label: "Secrets", icon: KeyRound },
  { id: "domains", label: "Domains", icon: Globe2 },
  { id: "deploys", label: "Deploys", icon: Rocket },
  { id: "settings", label: "Settings", icon: Settings },
];

export function AppDetail({ name }: { name: string }) {
  const [section, setSection] = useState<AppSection>("overview");
  const query = useQuery({
    queryKey: appKey(name),
    queryFn: () => getApp(name),
    refetchInterval: 5_000,
  });

  if (query.isPending) {
    return <p className="font-mono text-xs text-muted-foreground">Loading…</p>;
  }
  if (query.error) {
    const gone = query.error instanceof ApiError && query.error.status === 404;
    return (
      <section className="flex flex-col gap-4">
        {gone ? (
          <p className="text-sm text-muted-foreground">
            There is no app named <span className="font-mono">{name}</span>.
          </p>
        ) : (
          <ApiErrorAlert error={query.error} />
        )}
        <Link
          to="/apps"
          className="font-mono text-[13px] text-primary hover:underline"
        >
          Back to apps
        </Link>
      </section>
    );
  }

  const app = query.data;
  return (
    <section className="flex min-w-0 flex-col gap-4">
      <header className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="min-w-0">
          <h1 className="truncate font-display text-2xl font-medium tracking-tight text-white">
            {app.name}
          </h1>
          <p className="mt-1 truncate font-mono text-xs text-muted-foreground">
            {app.spec.source.github.repo}
          </p>
        </div>
        <StatusBadge conditions={app.status.conditions} />
        <div className="ml-auto flex items-center gap-2">
          {app.status.url ? (
            <Button asChild variant="ghost" size="sm">
              <a href={app.status.url} target="_blank" rel="noreferrer">
                Open app
              </a>
            </Button>
          ) : null}
        </div>
      </header>

      <LogsCard appName={app.name} />

      <DetailWorkspace
        sections={appSections}
        active={section}
        onSelect={setSection}
      >
        {section === "overview" ? <OverviewCard app={app} /> : null}
        {section === "variables" ? <VarsCard app={app} /> : null}
        {section === "secrets" ? <SecretsCard app={app} /> : null}
        {section === "domains" ? <DomainsCard appName={app.name} /> : null}
        {section === "deploys" ? <DeploysCard appName={app.name} /> : null}
        {section === "settings" ? <SettingsCard appName={app.name} /> : null}
      </DetailWorkspace>
    </section>
  );
}

function OverviewCard({ app }: { app: Awaited<ReturnType<typeof getApp>> }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Overview</CardTitle>
        <CardDescription>
          {app.spec.type ?? "Web"} app built with {app.spec.build.strategy}.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <dl className="grid grid-cols-[max-content_minmax(0,1fr)] items-baseline gap-x-6 gap-y-3">
          {app.status.url ? (
            <>
              <dt className="overline-label">URL</dt>
              <dd className="min-w-0 truncate font-mono text-[13px] text-primary">
                {app.status.url}
              </dd>
            </>
          ) : null}
          <dt className="overline-label">Replicas</dt>
          <dd className="font-mono text-[13px] text-foreground">
            {(app.status.availableReplicas ?? 0).toString()} of {(app.spec.replicas ?? 1).toString()} available
          </dd>
          <dt className="overline-label">Image</dt>
          <dd className="min-w-0 break-all font-mono text-xs text-foreground">
            {app.status.image ?? "no build rolled out yet"}
          </dd>
          <dt className="overline-label">Latest build</dt>
          <dd className="font-mono text-xs text-foreground">
            {app.status.latestBuild ?? "—"}
          </dd>
          <dt className="overline-label">Created</dt>
          <dd className="font-mono text-[13px] text-foreground">
            {formatAge(app.creationTimestamp)} ago
          </dd>
        </dl>
      </CardContent>
    </Card>
  );
}

function SettingsCard({ appName }: { appName: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Settings</CardTitle>
        <CardDescription>
          Change runtime settings, or remove the application.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col items-start gap-6">
        <Button asChild variant="outline">
          <Link to={`/apps/${encodeURIComponent(appName)}/edit`}>
            Edit runtime settings
          </Link>
        </Button>
        <div className="flex w-full flex-col gap-3 border-t pt-6">
          <div>
            <h2 className="font-display text-base font-medium text-destructive">
              Danger zone
            </h2>
            <p className="mt-1 text-sm text-muted-foreground">
              Deleting the app removes its running workload. Domains must be removed separately.
            </p>
          </div>
          <DeleteApp name={appName} />
        </div>
      </CardContent>
    </Card>
  );
}

function DeleteApp({ name }: { name: string }) {
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);

  const del = useMutation({
    mutationFn: () => deleteApp(name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: appsKey });
      navigate("/apps");
    },
  });

  if (!confirming) {
    return (
      <Button
        type="button"
        variant="destructive"
        size="sm"
        onClick={() => {
          setConfirming(true);
        }}
      >
        Delete app
      </Button>
    );
  }
  return (
    <div className="flex max-w-xl flex-col items-start gap-2">
      <span className="text-sm text-muted-foreground">
        Delete <span className="font-mono text-foreground">{name}</span> and its running workload?
      </span>
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          variant="destructive"
          size="sm"
          disabled={del.isPending}
          onClick={() => {
            del.mutate();
          }}
        >
          {del.isPending ? "Deleting…" : "Confirm delete"}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => {
            setConfirming(false);
            del.reset();
          }}
        >
          Cancel
        </Button>
      </div>
      <ApiErrorAlert error={del.error} />
      <StepUpGate
        error={del.error}
        onConfirmed={() => {
          del.mutate();
        }}
        onDismiss={() => {
          del.reset();
        }}
      />
    </div>
  );
}
