import { ArrowRight, Globe2, ServerCog } from "lucide-react";
import { useQuery } from "@tanstack/react-query";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { StatusBadge } from "@/components/StatusBadge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { appsKey, listApps, type AppResponse } from "@/lib/api";
import { formatAge, readiness } from "@/lib/format";
import { Link } from "@/lib/router";
import { cn } from "@/lib/utils";

export function AppList() {
  const query = useQuery({
    queryKey: appsKey,
    queryFn: listApps,
    refetchInterval: 10_000,
  });

  return (
    <section className="flex flex-col gap-6">
      <div className="flex items-end justify-between gap-4">
        <div className="flex flex-col gap-1">
          <h1 className="font-display text-3xl font-medium tracking-tight text-white">
            Apps
          </h1>
          {query.data ? (
            <p className="font-mono text-xs text-muted-foreground">
              {query.data.length.toString()} {query.data.length === 1 ? "application" : "applications"}
            </p>
          ) : null}
        </div>
        <Button asChild>
          <Link to="/apps/new">New app</Link>
        </Button>
      </div>
      {query.isPending ? (
        <p className="font-mono text-xs text-muted-foreground">Loading…</p>
      ) : null}
      <ApiErrorAlert error={query.error} />
      {query.data &&
        (query.data.length === 0 ? (
          <p className="rounded-lg border border-dashed border-primary/50 px-5 py-4 font-mono text-[13px] leading-relaxed text-primary">
            No apps yet — create one to deploy a repository.
          </p>
        ) : (
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
            {query.data.map((app) => (
              <AppCard key={app.name} app={app} />
            ))}
          </div>
        ))}
    </section>
  );
}

function AppCard({ app }: { app: AppResponse }) {
  const ready = readiness(app.status.conditions);
  return (
    <Link
      to={`/apps/${encodeURIComponent(app.name)}`}
      aria-label={app.name}
      className="group rounded-xl focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <Card
        className={cn(
          "relative h-full min-h-72 overflow-hidden transition-[border-color,transform,box-shadow] group-hover:-translate-y-0.5 group-hover:border-primary/35 group-hover:shadow-xl group-hover:shadow-black/20",
          "before:absolute before:inset-y-0 before:left-0 before:w-0.5",
          ready.tone === "ok"
            ? "before:bg-success"
            : ready.tone === "failed"
              ? "before:bg-destructive"
              : "before:bg-warning",
        )}
      >
        <CardHeader>
          <div className="flex items-start justify-between gap-3">
            <div className="flex min-w-0 flex-col gap-3">
              <CardTitle className="truncate text-xl">{app.name}</CardTitle>
              <StatusBadge conditions={app.status.conditions} />
            </div>
            <div className="flex items-center gap-2 font-mono text-xs text-muted-foreground">
              {app.spec.type === "Worker" ? (
                <ServerCog className="size-4" aria-hidden="true" />
              ) : (
                <Globe2 className="size-4" aria-hidden="true" />
              )}
              {app.spec.type ?? "Web"}
            </div>
          </div>
          <CardDescription className="min-h-10 break-all font-mono text-xs leading-relaxed">
            {app.status.url ?? ready.message ?? "Waiting for the first deployment."}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-3 border-t pt-5 font-mono text-xs">
            <dt className="text-muted-foreground">Replicas</dt>
            <dd className="text-right text-foreground">
              {(app.status.availableReplicas ?? 0).toString()} / {(app.spec.replicas ?? 1).toString()}
            </dd>
            <dt className="text-muted-foreground">Latest build</dt>
            <dd className="truncate text-right text-foreground">
              {app.status.latestBuild ?? "—"}
            </dd>
            <dt className="text-muted-foreground">Source</dt>
            <dd className="truncate text-right text-foreground" title={app.spec.source.github.repo}>
              {app.spec.source.github.repo}
            </dd>
            <dt className="text-muted-foreground">Age</dt>
            <dd className="text-right text-foreground">
              {formatAge(app.creationTimestamp)}
            </dd>
          </dl>
        </CardContent>
        <CardFooter className="mt-auto justify-end text-muted-foreground transition-colors group-hover:text-primary">
          <ArrowRight className="size-4" aria-hidden="true" />
        </CardFooter>
      </Card>
    </Link>
  );
}
