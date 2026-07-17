import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Database,
  Gauge,
  HardDrive,
  Plug,
  TriangleAlert,
} from "lucide-react";
import { useState, type FormEvent } from "react";

import { ResourceStreamCard } from "@/apps/LogsCard";
import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import {
  DetailWorkspace,
  type DetailSection,
} from "@/components/DetailWorkspace";
import { StatusBadge, StatusDot } from "@/components/StatusBadge";
import { StepUpGate } from "@/components/StepUpGate";
import { Badge } from "@/components/ui/badge";
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
  deleteMongo,
  getMongo,
  mongoExpressPath,
  mongoKey,
  mongoListKey,
  mongoLogsPath,
  updateMongo,
  updateMongoExpress,
  type MongoResponse,
} from "@/lib/api";
import { findCondition, formatAge, readiness } from "@/lib/format";
import { parseQuantityBytes } from "@/lib/quantity";
import { Link, navigate } from "@/lib/router";

type MongoSection =
  | "overview"
  | "connection"
  | "storage"
  | "express"
  | "danger";

const mongoSections: readonly DetailSection<MongoSection>[] = [
  { id: "overview", label: "Overview", icon: Gauge },
  { id: "connection", label: "Connection", icon: Plug },
  { id: "storage", label: "Storage", icon: HardDrive },
  { id: "express", label: "Mongo Express", icon: Database },
  { id: "danger", label: "Danger", icon: TriangleAlert, destructive: true },
];

export function MongoDetail({ name }: { name: string }) {
  const [section, setSection] = useState<MongoSection>("overview");
  const query = useQuery({
    queryKey: mongoKey(name),
    queryFn: () => getMongo(name),
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
          <p className="text-sm text-muted-foreground">
            There is no MongoDB database named{" "}
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

  const mongo = query.data;
  const ready = readiness(mongo.status.conditions);
  const failureMessage =
    ready.tone === "failed"
      ? findCondition(mongo.status.conditions, "Ready")?.message
      : undefined;

  return (
    <section className="flex min-w-0 flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <div className="min-w-0">
          <h1 className="truncate font-display text-2xl font-medium tracking-tight text-white">
            {mongo.name}
          </h1>
          <p className="mt-1 font-mono text-xs text-muted-foreground">
            MongoDB {mongo.spec.version ?? "8.0"} · {mongo.spec.storageSize ?? "10Gi"}
          </p>
        </div>
        <StatusBadge conditions={mongo.status.conditions} />
      </div>

      <ResourceStreamCard
        resourceName={mongo.name}
        path={mongoLogsPath(mongo.name)}
      />

      <DetailWorkspace
        sections={mongoSections}
        active={section}
        onSelect={setSection}
      >
        {section === "overview" ? (
          <MongoOverviewCard mongo={mongo} failureMessage={failureMessage} />
        ) : null}
        {section === "connection" ? (
          <MongoConnectionCard mongo={mongo} />
        ) : null}
        {section === "storage" ? (
          <Card>
            <CardHeader>
              <CardTitle>Storage</CardTitle>
              <CardDescription>
                Persistent storage can grow, but it cannot be reduced.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <GrowMongoStorage mongo={mongo} />
            </CardContent>
          </Card>
        ) : null}
        {section === "express" ? <MongoExpressCard mongo={mongo} /> : null}
        {section === "danger" ? (
          <Card>
            <CardHeader>
              <CardTitle className="text-destructive">Danger zone</CardTitle>
              <CardDescription>
                Deleting this database permanently removes its data volume.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <DeleteMongo name={mongo.name} />
            </CardContent>
          </Card>
        ) : null}
      </DetailWorkspace>
    </section>
  );
}

function MongoOverviewCard({
  mongo,
  failureMessage,
}: {
  mongo: MongoResponse;
  failureMessage?: string;
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Overview</CardTitle>
        <CardDescription>
          The major/LTS release line is fixed for this database.
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
            MongoDB {mongo.spec.version ?? "8.0"}
          </dd>
          <dt className="overline-label">Storage</dt>
          <dd className="font-mono text-[13px] text-foreground">
            {mongo.spec.storageSize ?? "10Gi"}
          </dd>
          <dt className="overline-label">Connection Secret</dt>
          <dd className="font-mono text-[13px] text-foreground">
            {mongo.status.secretName ?? mongo.name}
          </dd>
          <dt className="overline-label">Created</dt>
          <dd className="font-mono text-[13px] text-foreground">
            {formatAge(mongo.creationTimestamp)} ago
          </dd>
        </dl>
      </CardContent>
    </Card>
  );
}

const mongoConnectionKeys = ["uri", "host", "port", "database", "username", "password"];

function MongoConnectionCard({ mongo }: { mongo: MongoResponse }) {
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
            {mongo.status.secretName ?? mongo.name}
          </p>
        </div>
        <div>
          <p className="overline-label">Available keys</p>
          <div className="mt-2 flex flex-wrap gap-2">
            {mongoConnectionKeys.map((key) => (
              <code
                key={key}
                className="rounded-md border bg-terminal px-2 py-1 text-xs text-primary"
              >
                {key}
              </code>
            ))}
          </div>
        </div>
        <p className="text-sm leading-relaxed text-muted-foreground">
          For a full connection string, use variable{" "}
          <span className="font-mono text-foreground">MONGODB_URI</span> and
          select the <span className="font-mono text-foreground">uri</span> key
          in an app's Variables section.
        </p>
      </CardContent>
    </Card>
  );
}

function MongoExpressCard({ mongo }: { mongo: MongoResponse }) {
  const queryClient = useQueryClient();
  const enabled = mongo.spec.mongoExpress?.enabled ?? false;
  const condition = findCondition(
    mongo.status.conditions,
    "MongoExpressReady",
  );
  const ready = enabled && condition?.status === "True";
  const provisioning = enabled && condition?.reason === "Provisioning";
  const toggle = useMutation({
    mutationFn: (next: boolean) => updateMongoExpress(mongo.name, next),
    onSuccess: (updated) => {
      queryClient.setQueryData(mongoKey(mongo.name), updated);
      void queryClient.invalidateQueries({ queryKey: mongoListKey });
    },
  });

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-center gap-3">
          <CardTitle className="overline-label">Mongo Express</CardTitle>
          {!enabled ? (
            <Badge variant="secondary">
              <StatusDot />
              Disabled
            </Badge>
          ) : ready ? (
            <Badge variant="success">
              <StatusDot />
              Available
            </Badge>
          ) : provisioning ? (
            <Badge variant="warning">
              <StatusDot />
              Starting
            </Badge>
          ) : (
            <Badge variant="destructive" title={condition?.message}>
              <StatusDot />
              Unavailable
            </Badge>
          )}
        </div>
        <CardDescription>
          An optional, development-only browser for this database. It opens
          through your current Orkano session, without a second login or a
          credential in the URL.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <p className="text-sm leading-relaxed text-muted-foreground">
          Mongo Express is deprecated upstream. Enable it only on a trusted
          development installation, and disable it when you are finished.
        </p>
        {enabled && condition?.message && !ready ? (
          <p className="font-mono text-xs text-muted-foreground">
            {condition.message}
          </p>
        ) : null}
        <div className="flex flex-wrap items-center gap-2">
          {ready ? (
            <Button asChild size="sm">
              <a
                href={mongoExpressPath(mongo.name)}
                target="_blank"
                rel="noopener noreferrer"
              >
                Open Mongo Express
              </a>
            </Button>
          ) : null}
          <Button
            type="button"
            variant={enabled ? "outline" : "default"}
            size="sm"
            disabled={toggle.isPending}
            onClick={() => {
              toggle.mutate(!enabled);
            }}
          >
            {toggle.isPending
              ? enabled
                ? "Disabling…"
                : "Enabling…"
              : enabled
                ? "Disable Mongo Express"
                : "Enable Mongo Express"}
          </Button>
        </div>
        <ApiErrorAlert error={toggle.error} />
        <StepUpGate
          error={toggle.error}
          onConfirmed={() => {
            toggle.mutate(!enabled);
          }}
          onDismiss={() => {
            toggle.reset();
          }}
        />
      </CardContent>
    </Card>
  );
}

function GrowMongoStorage({ mongo }: { mongo: MongoResponse }) {
  const queryClient = useQueryClient();
  const current = mongo.spec.storageSize ?? "10Gi";
  const [size, setSize] = useState(current);
  const [sizeError, setSizeError] = useState("");
  const grow = useMutation({
    mutationFn: (storageSize: string) =>
      updateMongo(mongo.name, {
        version: mongo.spec.version,
        storageSize,
      }),
    onSuccess: (updated) => {
      queryClient.setQueryData(mongoKey(mongo.name), updated);
      void queryClient.invalidateQueries({ queryKey: mongoListKey });
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
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
          <Label htmlFor="mongo-grow">Grow storage</Label>
          <Input
            id="mongo-grow"
            className="w-32"
            value={size}
            onChange={(event) => {
              setSize(event.target.value);
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
      {sizeError ? (
        <p className="font-mono text-xs text-destructive">{sizeError}</p>
      ) : null}
      <ApiErrorAlert error={grow.error} />
    </form>
  );
}

function DeleteMongo({ name }: { name: string }) {
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () => deleteMongo(name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: mongoListKey });
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
        <span className="text-sm text-muted-foreground">
          Permanently delete{" "}
          <span className="font-mono text-foreground">{name}</span> and all of
          its data?
        </span>
        <Button
          type="button"
          variant="destructive"
          size="sm"
          disabled={remove.isPending}
          onClick={() => {
            remove.mutate();
          }}
        >
          {remove.isPending ? "Deleting…" : "Delete data"}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => {
            setConfirming(false);
            remove.reset();
          }}
        >
          Cancel
        </Button>
      </div>
      <ApiErrorAlert error={remove.error} />
      <StepUpGate
        error={remove.error}
        onConfirmed={() => {
          remove.mutate();
        }}
        onDismiss={() => {
          remove.reset();
        }}
      />
    </div>
  );
}
