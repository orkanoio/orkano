import { useQuery } from "@tanstack/react-query";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Button } from "@/components/ui/button";
import { appsKey, listApps, type AppResponse } from "@/lib/api";
import { readiness } from "@/lib/format";
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
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
            {query.data.map((app) => (
              <AppCard key={app.name} app={app} />
            ))}
          </div>
        ))}
    </section>
  );
}

// AppCard is deliberately minimal — a rectangle carrying the name and one
// status dot. Everything else (URL, replicas, source, builds) lives on the
// detail page one click away; the index answers only "what exists, and is it
// healthy" at a glance.
function AppCard({ app }: { app: AppResponse }) {
  const ready = readiness(app.status.conditions);
  return (
    <Link
      to={`/apps/${encodeURIComponent(app.name)}`}
      // aria-label short-circuits descendant content in the accessible-name
      // algorithm, so the status must ride the label itself — a nested
      // sr-only span would never reach a screen reader through this link.
      aria-label={`${app.name} — ${ready.label}`}
      className="group rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <div
        className="bg-card flex items-center gap-3 rounded-lg border px-4 py-3.5 transition-colors group-hover:border-primary/35"
        title={ready.message ?? ready.label}
      >
        <span
          aria-hidden="true"
          className={cn(
            "size-2 shrink-0 rounded-full",
            ready.tone === "ok"
              ? "bg-success"
              : ready.tone === "failed"
                ? "bg-destructive"
                : "bg-warning",
          )}
        />
        <span className="truncate font-mono text-sm text-foreground">
          {app.name}
        </span>
      </div>
    </Link>
  );
}
