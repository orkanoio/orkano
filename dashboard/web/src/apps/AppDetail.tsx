import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
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
import { ApiError, appKey, appsKey, deleteApp, getApp } from "@/lib/api";
import { formatAge } from "@/lib/format";
import { Link, navigate } from "@/lib/router";

import { DeploysCard } from "./DeploysCard";
import { DomainsCard } from "./DomainsCard";
import { EnvEditor } from "./EnvEditor";
import { LogsCard } from "./LogsCard";

export function AppDetail({ name }: { name: string }) {
  const query = useQuery({
    queryKey: appKey(name),
    queryFn: () => getApp(name),
    refetchInterval: 5_000,
  });

  if (query.isPending) {
    return <p className="text-muted-foreground text-sm">Loading…</p>;
  }
  if (query.error) {
    const gone =
      query.error instanceof ApiError && query.error.status === 404;
    return (
      <section className="flex flex-col gap-4">
        {gone ? (
          <p className="text-muted-foreground text-sm">
            There is no app named{" "}
            <span className="font-mono">{name}</span>.
          </p>
        ) : (
          <ApiErrorAlert error={query.error} />
        )}
        <Link to="/apps" className="text-primary text-sm hover:underline">
          Back to apps
        </Link>
      </section>
    );
  }

  const app = query.data;
  return (
    <section className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">{app.name}</h1>
        <StatusBadge conditions={app.status.conditions} />
        <div className="ml-auto flex gap-2">
          <Button asChild variant="outline" size="sm">
            <Link to={`/apps/${encodeURIComponent(app.name)}/edit`}>Edit</Link>
          </Button>
          <DeleteApp name={app.name} />
        </div>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Overview</CardTitle>
          <CardDescription>
            {app.spec.type ?? "Web"} app from{" "}
            <span className="font-mono">{app.spec.source.github.repo}</span>
            {app.spec.source.github.ref
              ? ` (${app.spec.source.github.ref})`
              : ""}
            , built with {app.spec.build.strategy}.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
            {app.status.url && (
              <>
                <dt className="text-muted-foreground">URL</dt>
                <dd>
                  <a
                    href={app.status.url}
                    target="_blank"
                    rel="noreferrer"
                    className="text-primary hover:underline"
                  >
                    {app.status.url}
                  </a>
                </dd>
              </>
            )}
            <dt className="text-muted-foreground">Replicas</dt>
            <dd>
              {(app.status.availableReplicas ?? 0).toString()} of{" "}
              {(app.spec.replicas ?? 1).toString()} available
            </dd>
            <dt className="text-muted-foreground">Image</dt>
            <dd className="font-mono text-xs break-all">
              {app.status.image ?? "no build rolled out yet"}
            </dd>
            {app.status.latestBuild && (
              <>
                <dt className="text-muted-foreground">Latest build</dt>
                <dd className="font-mono text-xs">{app.status.latestBuild}</dd>
              </>
            )}
            <dt className="text-muted-foreground">Created</dt>
            <dd>{formatAge(app.creationTimestamp)} ago</dd>
          </dl>
        </CardContent>
      </Card>
      <EnvEditor app={app} />
      <DomainsCard appName={app.name} />
      <DeploysCard appName={app.name} />
      <LogsCard appName={app.name} />
    </section>
  );
}

// DeleteApp is the two-step destructive control: an explicit confirm, then
// the step-up gate if the session's second factor is stale.
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
    <div className="flex flex-col items-end gap-2">
      <div className="flex items-center gap-2">
        <span className="text-sm">
          Delete <span className="font-mono">{name}</span> and its running
          workload? Domains pointing at it must be removed separately.
        </span>
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
