import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Field } from "@/components/Field";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  createMongo,
  createPostgres,
  mongoKey,
  mongoListKey,
  postgresKey,
  postgresListKey,
} from "@/lib/api";
import { parseQuantityBytes } from "@/lib/quantity";
import { Link, navigate } from "@/lib/router";

const nameRe = /^[a-z]([-a-z0-9]*[a-z0-9])?$/;
const postgresVersions = ["14", "15", "16", "17"] as const;
const gib = 2 ** 30;

type Engine = "postgres" | "mongo";

export function DatabaseForm() {
  const queryClient = useQueryClient();
  const [engine, setEngine] = useState<Engine>("postgres");
  const [name, setName] = useState("");
  const [postgresVersion, setPostgresVersion] = useState("16");
  const [storage, setStorage] = useState("10Gi");
  const [errors, setErrors] = useState<Record<string, string>>({});

  const create = useMutation({
    mutationFn: () => {
      const resourceName = name.trim();
      const spec = {
        version: engine === "mongo" ? "8.0" : postgresVersion,
        storageSize: storage.trim(),
      };
      return engine === "mongo"
        ? createMongo(resourceName, spec)
        : createPostgres(resourceName, spec);
    },
    onSuccess: (database) => {
      if (engine === "mongo") {
        queryClient.setQueryData(mongoKey(database.name), database);
        void queryClient.invalidateQueries({ queryKey: mongoListKey });
        navigate(`/databases/mongo/${encodeURIComponent(database.name)}`);
      } else {
        queryClient.setQueryData(postgresKey(database.name), database);
        void queryClient.invalidateQueries({ queryKey: postgresListKey });
        navigate(`/databases/${encodeURIComponent(database.name)}`);
      }
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const nextErrors: Record<string, string> = {};
    const resourceName = name.trim();
    if (resourceName.length > 63 || !nameRe.test(resourceName)) {
      nextErrors.name =
        "Start with a letter; lowercase letters, digits, and hyphens; at most 63 characters.";
    }
    const bytes = parseQuantityBytes(storage);
    if (bytes === null) {
      nextErrors.storage = "Enter a size like 10Gi.";
    } else if (bytes < gib) {
      nextErrors.storage = "At least 1Gi.";
    }
    setErrors(nextErrors);
    if (Object.keys(nextErrors).length === 0) {
      create.mutate();
    }
  };

  return (
    <section className="flex max-w-xl flex-col gap-6">
      <h1 className="font-display text-2xl font-medium tracking-tight text-white">
        New database
      </h1>
      <p className="text-muted-foreground text-sm leading-relaxed">
        Provision a persistent database and connect apps through a generated
        Kubernetes Secret. Credential values never enter Orkano&apos;s metadata
        database.
      </p>
      <form className="flex flex-col gap-6" onSubmit={submit}>
        <ApiErrorAlert error={create.error} />
        <Card>
          <CardContent className="flex flex-col gap-4">
            <Field id="database-engine" label="Engine">
              <Select
                id="database-engine"
                value={engine}
                onChange={(event) => {
                  setEngine(event.target.value as Engine);
                  create.reset();
                }}
              >
                <option value="postgres">PostgreSQL</option>
                <option value="mongo">MongoDB</option>
              </Select>
            </Field>
            <Field
              id="database-name"
              label="Name"
              error={errors.name}
              hint="Names are unique across apps and all catalog databases."
            >
              <Input
                id="database-name"
                value={name}
                onChange={(event) => {
                  setName(event.target.value);
                }}
                autoFocus
                required
              />
            </Field>
            <Field
              id="database-version"
              label="Version"
              hint="The release line cannot be changed later."
            >
              {engine === "mongo" ? (
                <Input id="database-version" value="MongoDB 8.0 (major/LTS)" disabled />
              ) : (
                <Select
                  id="database-version"
                  value={postgresVersion}
                  onChange={(event) => {
                    setPostgresVersion(event.target.value);
                  }}
                >
                  {postgresVersions.map((version) => (
                    <option key={version} value={version}>
                      PostgreSQL {version}
                    </option>
                  ))}
                </Select>
              )}
            </Field>
            <Field
              id="database-storage"
              label="Storage"
              error={errors.storage}
              hint="Can grow later, never shrink."
            >
              <Input
                id="database-storage"
                value={storage}
                onChange={(event) => {
                  setStorage(event.target.value);
                }}
                required
              />
            </Field>
          </CardContent>
        </Card>
        {engine === "mongo" ? (
          <p className="rounded-md border border-primary/20 bg-primary/5 px-4 py-3 text-sm text-muted-foreground">
            Connect your app with environment variable{" "}
            <span className="font-mono text-foreground">MONGODB_URI</span>,
            referencing this database&apos;s Secret key{" "}
            <span className="font-mono text-foreground">uri</span>.
          </p>
        ) : null}
        <div className="flex gap-3">
          <Button type="submit" disabled={create.isPending}>
            {create.isPending ? "Creating…" : "Create database"}
          </Button>
          <Button asChild variant="ghost">
            <Link to="/databases">Cancel</Link>
          </Button>
        </div>
      </form>
    </section>
  );
}
