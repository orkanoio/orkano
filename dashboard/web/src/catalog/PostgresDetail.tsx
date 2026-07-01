import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  ApiError,
  deletePostgres,
  getPostgres,
  postgresKey,
  postgresListKey,
  updatePostgres,
  type PostgresResponse,
} from "@/lib/api";
import { findCondition, formatAge, readiness } from "@/lib/format";
import { parseQuantityBytes } from "@/lib/quantity";
import { Link, navigate } from "@/lib/router";

// The connection Secret's frozen key set (ADR-0014; api/v1alpha1 SecretKey*).
const connectionKeys = [
  "uri",
  "host",
  "port",
  "database",
  "username",
  "password",
] as const;

export function PostgresDetail({ name }: { name: string }) {
  const query = useQuery({
    queryKey: postgresKey(name),
    queryFn: () => getPostgres(name),
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
            There is no database named{" "}
            <span className="font-mono">{name}</span>.
          </p>
        ) : (
          <ApiErrorAlert error={query.error} />
        )}
        <Link to="/databases" className="text-primary text-sm hover:underline">
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
    <section className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">{pg.name}</h1>
        <StatusBadge conditions={pg.status.conditions} />
        <div className="ml-auto">
          <DeletePostgres name={pg.name} />
        </div>
      </div>
      {failureMessage && (
        <p className="text-destructive text-sm">{failureMessage}</p>
      )}
      <Card>
        <CardHeader>
          <CardTitle>Overview</CardTitle>
          <CardDescription>
            PostgreSQL {pg.spec.version ?? "16"} — the major version is fixed
            for the life of the database.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
            <dt className="text-muted-foreground">Storage</dt>
            <dd>{pg.spec.storageSize ?? "10Gi"}</dd>
            <dt className="text-muted-foreground">Created</dt>
            <dd>{formatAge(pg.creationTimestamp)} ago</dd>
          </dl>
          <GrowStorage pg={pg} />
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Connect an app</CardTitle>
          <CardDescription>
            Connection details live only in the Kubernetes Secret{" "}
            <span className="font-mono">{pg.status.secretName ?? pg.name}</span>
            {" — "}reference it from an app's environment variables.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-2 text-sm">
          <p>
            On the app's page, add a secret reference with Secret name{" "}
            <span className="font-mono">
              {pg.status.secretName ?? pg.name}
            </span>{" "}
            and key <span className="font-mono">uri</span> (a full connection
            URI). Available keys:{" "}
            <span className="font-mono">{connectionKeys.join(", ")}</span>.
          </p>
        </CardContent>
      </Card>
    </section>
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
        <p className="text-destructive text-xs">{sizeError}</p>
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
        <span className="text-sm">
          Permanently delete <span className="font-mono">{name}</span> and all
          of its data?
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
