import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
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
import { Select } from "@/components/ui/select";
import {
  appKey,
  appsKey,
  setAppEnv,
  updateApp,
  type AppResponse,
  type EnvVar,
} from "@/lib/api";

// envNameRe mirrors the EnvVar name pattern (api/v1alpha1 shared_types) —
// also the server's Secret-key check (secrets.go).
const envNameRe = /^[A-Za-z_][A-Za-z0-9_]*$/;
const maxEnvVars = 64;

function envSecretName(app: string): string {
  return `${app}-env`;
}

// EnvEditor manages an App's environment in its two halves: spec.env entries
// (plaintext values + references to other Secrets, e.g. a Postgres connection
// Secret) written through the App spec, and the value-blind secret set written
// through PUT /env, which the dashboard can never read back (ADR-0013). The
// managed "<app>-env" references in spec.env belong to the second half and are
// preserved verbatim by the first.
export function EnvEditor({ app }: { app: AppResponse }) {
  return (
    <>
      <VarsCard app={app} />
      <SecretsCard app={app} />
    </>
  );
}

interface VarRow {
  kind: "value" | "ref";
  name: string;
  value: string;
  refName: string;
  refKey: string;
}

function isManagedRef(e: EnvVar, secretName: string): boolean {
  return e.secretRef?.name === secretName;
}

function varRowsFromSpec(app: AppResponse): VarRow[] {
  const secretName = envSecretName(app.name);
  return (app.spec.env ?? [])
    .filter((e) => !isManagedRef(e, secretName))
    .map((e) =>
      e.secretRef
        ? {
            kind: "ref" as const,
            name: e.name,
            value: "",
            refName: e.secretRef.name,
            refKey: e.secretRef.key,
          }
        : { kind: "value" as const, name: e.name, value: e.value ?? "", refName: "", refKey: "" },
    );
}

function managedRefs(app: AppResponse): EnvVar[] {
  const secretName = envSecretName(app.name);
  return (app.spec.env ?? []).filter((e) => isManagedRef(e, secretName));
}

function VarsCard({ app }: { app: AppResponse }) {
  const queryClient = useQueryClient();
  const [rows, setRows] = useState<VarRow[]>(() => varRowsFromSpec(app));
  const [error, setError] = useState("");

  const save = useMutation({
    mutationFn: () => {
      const env: EnvVar[] = [
        ...rows.map((r) =>
          r.kind === "value"
            ? { name: r.name.trim(), value: r.value }
            : {
                name: r.name.trim(),
                secretRef: { name: r.refName.trim(), key: r.refKey.trim() },
              },
        ),
        ...managedRefs(app),
      ];
      return updateApp(app.name, { ...app.spec, env });
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(appKey(app.name), updated);
      void queryClient.invalidateQueries({ queryKey: appsKey });
    },
  });

  const submit = () => {
    const err = validateVarRows(rows, app);
    setError(err);
    if (err === "") {
      save.mutate();
    }
  };

  const update = (i: number, patch: Partial<VarRow>) => {
    setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Environment variables</CardTitle>
        <CardDescription>
          Plain values and references to Kubernetes Secrets (a database's
          connection Secret, for example). Secret values themselves live in the
          section below.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {error !== "" && <p className="text-destructive text-sm">{error}</p>}
        <ApiErrorAlert error={save.error} />
        {rows.length === 0 && (
          <p className="text-muted-foreground text-sm">No variables.</p>
        )}
        {rows.map((row, i) => (
          <div key={i.toString()} className="flex flex-wrap items-center gap-2">
            <Input
              aria-label="Variable name"
              className="w-44 font-mono"
              placeholder="NAME"
              value={row.name}
              onChange={(e) => {
                update(i, { name: e.target.value });
              }}
            />
            <Select
              aria-label="Variable kind"
              className="w-36"
              value={row.kind}
              onChange={(e) => {
                update(i, { kind: e.target.value as VarRow["kind"] });
              }}
            >
              <option value="value">Value</option>
              <option value="ref">Secret reference</option>
            </Select>
            {row.kind === "value" ? (
              <Input
                aria-label="Variable value"
                className="min-w-40 flex-1 font-mono"
                value={row.value}
                onChange={(e) => {
                  update(i, { value: e.target.value });
                }}
              />
            ) : (
              <>
                <Input
                  aria-label="Secret name"
                  className="w-44 font-mono"
                  placeholder="secret name"
                  value={row.refName}
                  onChange={(e) => {
                    update(i, { refName: e.target.value });
                  }}
                />
                <Input
                  aria-label="Secret key"
                  className="w-32 font-mono"
                  placeholder="key"
                  value={row.refKey}
                  onChange={(e) => {
                    update(i, { refKey: e.target.value });
                  }}
                />
              </>
            )}
            <Button
              type="button"
              variant="ghost"
              size="sm"
              aria-label={`Remove variable ${(i + 1).toString()}`}
              onClick={() => {
                setRows((rs) => rs.filter((_, j) => j !== i));
              }}
            >
              Remove
            </Button>
          </div>
        ))}
        <div className="flex gap-3">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => {
              setRows((rs) => [
                ...rs,
                { kind: "value", name: "", value: "", refName: "", refKey: "" },
              ]);
            }}
          >
            Add variable
          </Button>
          <Button
            type="button"
            onClick={submit}
            disabled={save.isPending}
            size="sm"
          >
            {save.isPending ? "Saving…" : "Save variables"}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

function validateVarRows(rows: VarRow[], app: AppResponse): string {
  const seen = new Set<string>();
  const secretName = envSecretName(app.name);
  for (const row of rows) {
    const name = row.name.trim();
    if (name.length > 253 || !envNameRe.test(name)) {
      return "Variable names must start with a letter or underscore and use only letters, digits, and underscores.";
    }
    if (seen.has(name)) {
      return `Duplicate variable name ${name}.`;
    }
    seen.add(name);
    // An empty value cannot be stored: the Go type omits it (omitempty) and
    // the CEL exactly-one-of rule then rejects the entry with an opaque 422.
    if (row.kind === "value" && row.value === "") {
      return `${name} needs a value.`;
    }
    if (row.kind === "ref") {
      if (row.refName.trim() === "" || row.refKey.trim() === "") {
        return `Reference ${name} needs a Secret name and key.`;
      }
      if (row.refName.trim() === secretName) {
        return "Use the secret values section below for this app's own secrets.";
      }
    }
  }
  const managed = managedRefs(app);
  for (const e of managed) {
    if (seen.has(e.name)) {
      return `${e.name} is managed in the secret values section — remove it here.`;
    }
  }
  if (rows.length + managed.length > maxEnvVars) {
    return "An app can have at most 64 environment variables.";
  }
  return "";
}

interface SecretRow {
  key: string;
  value: string;
}

function SecretsCard({ app }: { app: AppResponse }) {
  const queryClient = useQueryClient();
  const [rows, setRows] = useState<SecretRow[]>(() =>
    managedRefs(app).map((e) => ({ key: e.name, value: "" })),
  );
  const [error, setError] = useState("");

  const save = useMutation({
    mutationFn: (secrets: Record<string, string>) =>
      setAppEnv(app.name, secrets),
    onSuccess: (updated) => {
      queryClient.setQueryData(appKey(app.name), updated);
      void queryClient.invalidateQueries({ queryKey: appsKey });
    },
  });

  const submit = () => {
    const err = validateSecretRows(rows, app);
    setError(err);
    if (err === "") {
      save.mutate(Object.fromEntries(rows.map((r) => [r.key.trim(), r.value])));
    }
  };

  const update = (i: number, patch: Partial<SecretRow>) => {
    setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Secret values</CardTitle>
        <CardDescription>
          Stored only in the Kubernetes Secret{" "}
          <span className="font-mono">{envSecretName(app.name)}</span>. Values
          are write-only — the dashboard cannot read them back, and saving
          replaces the whole set with what is entered here.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {error !== "" && <p className="text-destructive text-sm">{error}</p>}
        <ApiErrorAlert error={save.error} />
        {rows.length === 0 && (
          <p className="text-muted-foreground text-sm">No secret values.</p>
        )}
        {rows.map((row, i) => (
          <div key={i.toString()} className="flex flex-wrap items-center gap-2">
            <Input
              aria-label="Secret variable name"
              className="w-44 font-mono"
              placeholder="NAME"
              value={row.key}
              onChange={(e) => {
                update(i, { key: e.target.value });
              }}
            />
            <Input
              aria-label="Secret variable value"
              className="min-w-40 flex-1 font-mono"
              type="password"
              autoComplete="new-password"
              value={row.value}
              onChange={(e) => {
                update(i, { value: e.target.value });
              }}
            />
            <Button
              type="button"
              variant="ghost"
              size="sm"
              aria-label={`Remove secret ${(i + 1).toString()}`}
              onClick={() => {
                setRows((rs) => rs.filter((_, j) => j !== i));
              }}
            >
              Remove
            </Button>
          </div>
        ))}
        <div className="flex gap-3">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => {
              setRows((rs) => [...rs, { key: "", value: "" }]);
            }}
          >
            Add secret
          </Button>
          <Button
            type="button"
            onClick={submit}
            disabled={save.isPending}
            size="sm"
          >
            {save.isPending ? "Saving…" : "Save secrets"}
          </Button>
        </div>
        <StepUpGate
          error={save.error}
          onConfirmed={() => {
            if (save.variables) {
              save.mutate(save.variables);
            }
          }}
          onDismiss={() => {
            save.reset();
          }}
        />
      </CardContent>
    </Card>
  );
}

function validateSecretRows(rows: SecretRow[], app: AppResponse): string {
  const keys = new Set<string>();
  for (const row of rows) {
    const key = row.key.trim();
    if (key.length > 253 || !envNameRe.test(key)) {
      return "Secret names must start with a letter or underscore and use only letters, digits, and underscores.";
    }
    if (keys.has(key)) {
      return `Duplicate secret name ${key}.`;
    }
    keys.add(key);
  }
  // The server reconciles spec.env to: every non-managed entry whose name is
  // not taken over by a secret key, plus the secret keys.
  const secretName = envSecretName(app.name);
  const preserved = (app.spec.env ?? []).filter(
    (e) => !isManagedRef(e, secretName) && !keys.has(e.name),
  );
  if (keys.size + preserved.length > maxEnvVars) {
    return "An app can have at most 64 environment variables.";
  }
  return "";
}
