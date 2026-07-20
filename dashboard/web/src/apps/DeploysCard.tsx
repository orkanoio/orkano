import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, Rocket, TriangleAlert } from "lucide-react";
import { useEffect, useState } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  appBuildsKey,
  appKey,
  buildLogsPath,
  deployApp,
  listAppBuilds,
  type BuildPhase,
  type BuildResponse,
} from "@/lib/api";
import { findCondition, formatAge } from "@/lib/format";
import { Link } from "@/lib/router";

import { ResourceStreamCard } from "./LogsCard";

export function DeploysCard({ appName }: { appName: string }) {
  const client = useQueryClient();
  const [selectedName, setSelectedName] = useState("");
  const [queued, setQueued] = useState(false);
  const query = useQuery({
    queryKey: appBuildsKey(appName),
    queryFn: () => listAppBuilds(appName),
    refetchInterval: (current) =>
      current.state.data?.items.some((build) => !isTerminal(build.status.phase))
        ? 2_000
        : 10_000,
  });
  const deploy = useMutation({
    mutationFn: async () => {
      const [result] = await Promise.allSettled([
        deployApp(appName),
        new Promise<void>((resolve) => window.setTimeout(resolve, 300)),
      ]);
      if (result.status === "rejected") {
        throw result.reason;
      }
    },
    onSuccess: () => {
      setQueued(true);
      void client.invalidateQueries({ queryKey: appBuildsKey(appName) });
      void client.invalidateQueries({ queryKey: appKey(appName) });
    },
  });

  const builds = query.data?.items ?? [];
  useEffect(() => {
    if (builds.length === 0) {
      setSelectedName("");
      return;
    }
    const first = builds[0];
    if (first && !builds.some((build) => build.name === selectedName)) {
      setSelectedName(first.name);
    }
  }, [builds, selectedName]);
  const selected = builds.find((build) => build.name === selectedName);

  return (
    <Card>
      <CardHeader className="grid-cols-[1fr_auto] items-start gap-4">
        <div>
          <CardTitle>Build history</CardTitle>
          <CardDescription>
            Every Dockerfile attempt, its result, and the BuildKit output.
          </CardDescription>
        </div>
        <Button
          type="button"
          disabled={deploy.isPending}
          onClick={() => {
            setQueued(false);
            deploy.mutate();
          }}
        >
          <Rocket data-icon="inline-start" aria-hidden="true" />
          {deploy.isPending ? "Requesting deploy…" : "Deploy now"}
        </Button>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <ApiErrorAlert error={query.error ?? deploy.error} />
        {queued ? (
          <Alert variant="success" aria-live="polite">
            <CheckCircle2 aria-hidden="true" />
            <AlertDescription>
              Deploy requested. Orkano is resolving the tracked ref; the new
              Build will appear here shortly.
            </AlertDescription>
          </Alert>
        ) : null}
        {query.data && !query.data.automaticDeploys ? (
          <Alert>
            <TriangleAlert className="text-warning" aria-hidden="true" />
            <AlertDescription>
              <p>
                Automatic push deploys are off for{" "}
                <code className="font-mono text-xs text-foreground">
                  {query.data.repo}
                </code>
                . Manual deploys still work.
              </p>
              <p>
                Add{" "}
                <code className="font-mono text-xs text-foreground">
                  --allow-repo {query.data.repo}
                </code>{" "}
                to your original <code className="font-mono text-xs">orkano init</code>{" "}
                command, or add it to Helm&apos;s{" "}
                <code className="font-mono text-xs text-foreground">
                  repoAllowlist
                </code>
                . <Link to="/setup" className="text-primary hover:underline">See Setup</Link>.
              </p>
            </AlertDescription>
          </Alert>
        ) : null}

        {query.data && builds.length === 0 ? (
          <div className="border-primary/50 rounded-lg border border-dashed px-5 py-4">
            <p className="font-mono text-[13px] leading-relaxed text-primary">
              No Build has started for this app.
            </p>
            <p className="mt-1 text-sm text-muted-foreground">
              New apps queue their first build automatically. If the request was
              missed, use Deploy now to build the current tracked ref.
            </p>
          </div>
        ) : null}

        {builds.length > 0 ? (
          <div className="overflow-x-auto rounded-lg border">
            <Table aria-label="Build attempts">
              <TableHeader>
                <TableRow>
                  <TableHead>Build</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Commit</TableHead>
                  <TableHead>Started</TableHead>
                  <TableHead>Duration</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {builds.map((build) => (
                  <TableRow
                    key={build.name}
                    data-state={selectedName === build.name ? "selected" : undefined}
                  >
                    <TableCell>
                      <button
                        type="button"
                        className="max-w-56 truncate text-left font-mono text-xs text-primary hover:underline"
                        title={build.name}
                        onClick={() => setSelectedName(build.name)}
                      >
                        {build.name}
                      </button>
                    </TableCell>
                    <TableCell>
                      <BuildPhaseBadge phase={build.status.phase} />
                    </TableCell>
                    <TableCell className="font-mono text-xs" title={build.spec.commit}>
                      {build.spec.commit.slice(0, 8)}
                    </TableCell>
                    <TableCell
                      className="font-mono text-xs text-muted-foreground"
                      title={build.status.startedAt ?? build.creationTimestamp ?? undefined}
                    >
                      {formatAge(build.status.startedAt ?? build.creationTimestamp)}
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {formatDuration(build.status.startedAt, build.status.completedAt)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        ) : null}

        {selected ? <SelectedBuild appName={appName} build={selected} /> : null}
      </CardContent>
    </Card>
  );
}

function SelectedBuild({
  appName,
  build,
}: {
  appName: string;
  build: BuildResponse;
}) {
  const completed = findCondition(build.status.conditions, "Completed");
  const explanation = completed?.message ?? completed?.reason;
  const terminal = isTerminal(build.status.phase);

  return (
    <section className="flex flex-col gap-3 border-t pt-4" aria-label="Selected build">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="font-display text-base font-medium text-foreground">
          Selected attempt
        </h3>
        <span className="font-mono text-xs text-muted-foreground">{build.name}</span>
        <BuildPhaseBadge phase={build.status.phase} />
      </div>
      {explanation ? (
        <p
          className={
            build.status.phase === "Failed"
              ? "rounded-lg border border-destructive/30 bg-destructive/8 px-4 py-3 text-sm text-destructive"
              : "text-sm text-muted-foreground"
          }
        >
          {explanation}
        </p>
      ) : null}
      {build.status.image ? (
        <p className="break-all font-mono text-[11px] text-muted-foreground">
          Image: {build.status.image}
        </p>
      ) : null}
      {build.status.jobRef ? (
        <ResourceStreamCard
          key={build.name}
          resourceName={build.name}
          path={buildLogsPath(appName, build.name, {
            follow: !terminal,
            tail: 5000,
          })}
          title="BuildKit output"
          sticky={false}
          endedLabel={terminal ? "Output complete" : "Stream ended"}
          reconnectOnEnd={!terminal}
        />
      ) : (
        <div className="bg-terminal rounded-lg border px-4 py-5 font-mono text-xs text-muted-foreground">
          {build.status.phase === "Failed"
            ? "This attempt failed before a BuildKit Job was created."
            : "Waiting for the operator to create the BuildKit Job…"}
        </div>
      )}
    </section>
  );
}

function BuildPhaseBadge({ phase }: { phase: BuildPhase | undefined }) {
  switch (phase) {
    case "Succeeded":
      return <Badge variant="success">Succeeded</Badge>;
    case "Failed":
      return <Badge variant="destructive">Failed</Badge>;
    case "Running":
      return <Badge variant="warning">Running</Badge>;
    case "Pending":
      return <Badge variant="secondary">Pending</Badge>;
    default:
      return <Badge variant="secondary">Queued</Badge>;
  }
}

function isTerminal(phase: BuildPhase | undefined): boolean {
  return phase === "Succeeded" || phase === "Failed";
}

function formatDuration(start: string | undefined, end: string | undefined): string {
  if (!start) {
    return "—";
  }
  const startMs = Date.parse(start);
  const endMs = end ? Date.parse(end) : Date.now();
  if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs < startMs) {
    return "—";
  }
  const seconds = Math.floor((endMs - startMs) / 1000);
  if (seconds < 60) {
    return `${seconds.toString()}s`;
  }
  return `${Math.floor(seconds / 60).toString()}m ${String(seconds % 60).padStart(2, "0")}s`;
}
