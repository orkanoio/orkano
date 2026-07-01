import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Field } from "@/components/Field";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { createPostgres, postgresKey, postgresListKey } from "@/lib/api";
import { parseQuantityBytes } from "@/lib/quantity";
import { Link, navigate } from "@/lib/router";

// DNS-1035, stricter than the server's DNS-1123 create check: the reconciler
// derives the SQL identifier and Service name from it, so a leading digit or a
// dot passes create and then sticks at ProvisionFailed. Enforce the real rule
// up front.
const nameRe = /^[a-z]([-a-z0-9]*[a-z0-9])?$/;

export const postgresVersions = ["14", "15", "16", "17"] as const;

const gib = 2 ** 30;

export function PostgresForm() {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [version, setVersion] = useState("16");
  const [storage, setStorage] = useState("10Gi");
  const [errors, setErrors] = useState<Record<string, string>>({});

  const create = useMutation({
    mutationFn: () =>
      createPostgres(name.trim(), {
        version,
        storageSize: storage.trim(),
      }),
    onSuccess: (pg) => {
      queryClient.setQueryData(postgresKey(pg.name), pg);
      void queryClient.invalidateQueries({ queryKey: postgresListKey });
      navigate(`/databases/${encodeURIComponent(pg.name)}`);
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const errs: Record<string, string> = {};
    const n = name.trim();
    if (n.length > 63 || !nameRe.test(n)) {
      errs.name =
        "Start with a letter; lowercase letters, digits, and hyphens; at most 63 characters.";
    }
    const bytes = parseQuantityBytes(storage);
    if (bytes === null) {
      errs.storage = "Enter a size like 10Gi.";
    } else if (bytes < gib) {
      errs.storage = "At least 1Gi.";
    }
    setErrors(errs);
    if (Object.keys(errs).length === 0) {
      create.mutate();
    }
  };

  return (
    <section className="flex max-w-xl flex-col gap-4">
      <h1 className="text-xl font-semibold">New database</h1>
      <p className="text-muted-foreground text-sm">
        A PostgreSQL instance with persistent storage. Its connection details
        land in a Kubernetes Secret named after the database, ready to
        reference from an app's environment.
      </p>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <ApiErrorAlert error={create.error} />
        <Field
          id="pg-name"
          label="Name"
          error={errors.name}
          hint="Also names the connection Secret and the database itself."
        >
          <Input
            id="pg-name"
            value={name}
            onChange={(e) => {
              setName(e.target.value);
            }}
            autoFocus
            required
          />
        </Field>
        <Field
          id="pg-version"
          label="Version"
          hint="The major version cannot be changed later."
        >
          <Select
            id="pg-version"
            value={version}
            onChange={(e) => {
              setVersion(e.target.value);
            }}
          >
            {postgresVersions.map((v) => (
              <option key={v} value={v}>
                PostgreSQL {v}
              </option>
            ))}
          </Select>
        </Field>
        <Field
          id="pg-storage"
          label="Storage"
          error={errors.storage}
          hint="Can grow later, never shrink."
        >
          <Input
            id="pg-storage"
            value={storage}
            onChange={(e) => {
              setStorage(e.target.value);
            }}
            required
          />
        </Field>
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
