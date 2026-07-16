import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Gauge, HardDrive, Plug, TriangleAlert } from "lucide-react";
import { useState, type FormEvent } from "react";

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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  ApiError,
  deletePostgres,
  getPostgres,
  postgresKey,
  postgresListKey,
  postgresLogsPath,
  updatePostgres,
  type PostgresResponse,
} from "@/lib/api";
import { findCondition, formatAge, readiness } from "@/lib/format";
import { parseQuantityBytes } from "@/lib/quantity";
import { Link, navigate } from "@/lib/router";
import { ResourceStreamCard } from "@/apps/LogsCard";

type PostgresSection = "overview" | "connection" | "storage" | "danger";

const postgresSections: readonly DetailSection<PostgresSection>[] = [
  { id: "overview", label: "Overview", icon: Gauge },
  { id: "connection", label: "Connection", icon: Plug },
  { id: "storage", label: "Storage", icon: HardDrive },
  { id: "danger", label: "Danger", icon: TriangleAlert, destructive: true },
];

export function PostgresDetail({ name }: { name: string }) {
  const [section, setSection] = useState<PostgresSection>("overview");
  const query = useQuery({
    queryKey: postgresKey(name),
    queryFn: () => getPostgres(name),
    refetchInterval: 5_000,
  });

  if (query.isPending) {
    return <p className="font-mono text-xs text-muted-foreground">Loading…</p>;
  }
  if (query.error) {
    const gone =
      query.error instanceof ApiError && query.error.status === 404;
    return (
      <section className="flex flex-col gap-4">
        {gone ? (
          <p className="text-muted-foreground text-sm">
            There is no database named{" "}
            <span className="font-mono text-foreground">{name}</span>.
          </p>
        ) : (
          <ApiErrorAlert error={query.error} />
        )}
        <Link
          to="/databases"
          className="font-mono text-[13px] text-primary hover:underline"
        >
          Back to databases
        </Link>
      </section>
    );
  }

  const pg = query.data;
  const ready = readiness(pg.status.conditions);
  const failureMessage =
    ready.tone === "failed"
      ? findCondition(pg.status.conditions, "Ready")?.message
      : undefined;

  return (
    <section className="flex min-w-0 flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <div className="min-w-0">
          <h1 className="truncate font-display text-2xl font-medium tracking-tight text-white">
            {pg.name}
          </h1>
          <p className="mt-1 font-mono text-xs text-muted-foreground">
            PostgreSQL {pg.spec.version ?? "16"} · {pg.spec.storageSize ?? "10Gi"}
          </p>
        </div>
        <StatusBadge conditions={pg.status.conditions} />
      </div>

      <ResourceStreamCard
        resourceName={pg.name}
        path={postgresLogsPath(pg.name)}
      />

      <DetailWorkspace
        sections={postgresSections}
        active={section}
        onSelect={setSection}
      >
        {section === "overview" ? (
          <PostgresOverviewCard pg={pg} failureMessage={failureMessage} />
        ) : null}
        {section === "connection" ? <PostgresConnectionCard pg={pg} /> : null}
        {section === "storage" ? (
          <Card>
            <CardHeader>
              <CardTitle>Storage</CardTitle>
              <CardDescription>
                Persistent storage can grow, but it cannot be reduced.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <GrowStorage pg={pg} />
            </CardContent>
          </Card>
        ) : null}
        {section === "danger" ? (
          <Card>
            <CardHeader>
              <CardTitle className="text-destructive">Danger zone</CardTitle>
              <CardDescription>
                Deleting this database permanently removes its data volume.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <DeletePostgres name={pg.name} />
            </CardContent>
          </Card>
        ) : null}
      </DetailWorkspace>
    </section>
  );
}

function PostgresOverviewCard({
  pg,
  failureMessage,
}: {
  pg: PostgresResponse;
  failureMessage?: string;
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Overview</CardTitle>
        <CardDescription>
          The major version is fixed for the life of this database.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {failureMessage ? (
          <p className="font-mono text-[13px] text-destructive">
            {failureMessage}
          </p>
        ) : null}
        <dl className="grid grid-cols-[max-content_1fr] items-baseline gap-x-6 gap-y-3">
          <dt className="overline-label">Engine</dt>
          <dd className="font-mono text-[13px] text-foreground">
            PostgreSQL {pg.spec.version ?? "16"}
          </dd>
          <dt className="overline-label">Storage</dt>
          <dd className="font-mono text-[13px] text-foreground">
            {pg.spec.storageSize ?? "10Gi"}
          </dd>
          <dt className="overline-label">Connection Secret</dt>
          <dd className="font-mono text-[13px] text-foreground">
            {pg.status.secretName ?? pg.name}
          </dd>
          <dt className="overline-label">Created</dt>
          <dd className="font-mono text-[13px] text-foreground">
            {formatAge(pg.creationTimestamp)} ago
          </dd>
        </dl>
      </CardContent>
    </Card>
  );
}

const connectionKeys = ["uri", "host", "port", "database", "username", "password"];

function PostgresConnectionCard({ pg }: { pg: PostgresResponse }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Connection</CardTitle>
        <CardDescription>
          Reference this Kubernetes Secret from an app. Orkano never reads the values back.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div>
          <p className="overline-label">Connection Secret</p>
          <p className="mt-1 font-mono text-sm text-foreground">
            {pg.status.secretName ?? pg.name}
          </p>
        </div>
        <div>
          <p className="overline-label">Available keys</p>
          <div className="mt-2 flex flex-wrap gap-2">
            {connectionKeys.map((key) => (
              <code key={key} className="rounded-md border bg-terminal px-2 py-1 text-xs text-primary">
                {key}
              </code>
            ))}
          </div>
        </div>
        <p className="text-sm leading-relaxed text-muted-foreground">
          For a full connection string, select this database and the <span className="font-mono text-foreground">uri</span> key in an app's Variables section.
        </p>
      </CardContent>
    </Card>
  );
}

function GrowStorage({ pg }: { pg: PostgresResponse }) {
  const queryClient = useQueryClient();
  const current = pg.spec.storageSize ?? "10Gi";
  const [size, setSize] = useState(current);
  const [sizeError, setSizeError] = useState("");

  const grow = useMutation({
    // version rides along: the update replaces the whole spec, and an omitted
    // version would be re-defaulted by the apiserver — a CEL immutability
    // rejection for any database not on the default version.
    mutationFn: (storageSize: string) =>
      updatePostgres(pg.name, { version: pg.spec.version, storageSize }),
    onSuccess: (updated) => {
      queryClient.setQueryData(postgresKey(pg.name), updated);
      void queryClient.invalidateQueries({ queryKey: postgresListKey });
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const next = size.trim();
    const nextBytes = parseQuantityBytes(next);
    if (nextBytes === null) {
      setSizeError("Enter a size like 20Gi.");
      return;
    }
    const currentBytes = parseQuantityBytes(current);
    if (currentBytes !== null && nextBytes < currentBytes) {
      setSizeError(`Storage can only grow — currently ${current}.`);
      return;
    }
    setSizeError("");
    grow.mutate(next);
  };

  return (
    <form className="flex flex-col gap-2" onSubmit={submit}>
      <div className="flex flex-wrap items-end gap-2">
        <div className="flex flex-col gap-2">
          <Label htmlFor="pg-grow">Grow storage</Label>
          <Input
            id="pg-grow"
            className="w-32"
            value={size}
            onChange={(e) => {
              setSize(e.target.value);
            }}
          />
        </div>
        <Button
          type="submit"
          variant="outline"
          size="sm"
          disabled={grow.isPending || size.trim() === current}
        >
          {grow.isPending ? "Resizing…" : "Resize"}
        </Button>
      </div>
      {sizeError !== "" && (
        <p className="font-mono text-xs text-destructive">{sizeError}</p>
      )}
      <ApiErrorAlert error={grow.error} />
    </form>
  );
}

// DeletePostgres deletes the database AND ITS DATA (the data PVC cascades,
// ADR-0014) — explicit confirm plus the step-up gate.
function DeletePostgres({ name }: { name: string }) {
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);

  const del = useMutation({
    mutationFn: () => deletePostgres(name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: postgresListKey });
      navigate("/databases");
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
        Delete database
      </Button>
    );
  }
  return (
    <div className="flex flex-col items-end gap-2">
      <div className="flex items-center gap-2">
        <span className="text-muted-foreground text-sm">
          Permanently delete{" "}
          <span className="font-mono text-foreground">{name}</span> and all of
          its data?
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
          {del.isPending ? "Deleting…" : "Delete data"}
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
