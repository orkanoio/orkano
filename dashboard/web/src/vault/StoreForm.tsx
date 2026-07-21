import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Field } from "@/components/Field";
import { StepUpGate } from "@/components/StepUpGate";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  createSecretStore,
  listSecretStores,
  secretStoresKey,
  updateSecretStore,
} from "@/lib/api";
import { Link, navigate } from "@/lib/router";

// The server's validResourceName, minus the reserved suffixes it also
// refuses; mirrored here for an inline message instead of a 400 round-trip.
const nameRe = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;

// isHTTPSURL mirrors the server's url.Parse check: a scheme prefix alone
// ("https://") is not a server.
function isHTTPSURL(raw: string): boolean {
  try {
    const u = new URL(raw);
    return u.protocol === "https:" && u.host !== "";
  } catch {
    return false;
  }
}

// StoreForm connects a new store or rotates an existing one (edit set).
// Vault is the documented stable flagship (ADR-0018 decision 5); other ESO
// providers work by authoring their SecretStore with kubectl and show up on
// the vault page like any other store.
//
// Rotation MUST seed from the live store: the server replaces the whole spec
// (never merges), so a form initialized with defaults would silently rewrite
// a store's real path/version on a routine token rotation.
export function StoreForm({ edit }: { edit?: string }) {
  if (!edit) {
    return <StoreFormInner />;
  }
  return <RotateLoader name={edit} />;
}

function RotateLoader({ name }: { name: string }) {
  const stores = useQuery({
    queryKey: secretStoresKey,
    queryFn: listSecretStores,
  });
  if (stores.isPending) {
    return <p className="font-mono text-xs text-muted-foreground">Loading…</p>;
  }
  const store = stores.data?.find((s) => s.name === name);
  if (!store) {
    return (
      <section className="flex flex-col gap-2">
        <ApiErrorAlert error={stores.error} />
        <p className="text-muted-foreground text-sm">
          Store {name} was not found — it may have been disconnected.{" "}
          <Link to="/vault" className="text-primary hover:underline">
            Back to the vault page.
          </Link>
        </p>
      </section>
    );
  }
  return (
    <StoreFormInner
      edit={name}
      initial={{
        server: store.server ?? "",
        path: store.path ?? "secret",
        version: store.version ?? "v2",
      }}
    />
  );
}

function StoreFormInner({
  edit,
  initial,
}: {
  edit?: string;
  initial?: { server: string; path: string; version: string };
}) {
  const queryClient = useQueryClient();
  const [name, setName] = useState(edit ?? "");
  const [server, setServer] = useState(initial?.server ?? "");
  const [path, setPath] = useState(initial?.path ?? "secret");
  const [version, setVersion] = useState(initial?.version ?? "v2");
  const [token, setToken] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});

  const save = useMutation({
    mutationFn: () => {
      const body = {
        vault: { server: server.trim(), path: path.trim(), version },
        token,
      };
      return edit
        ? updateSecretStore(edit, body)
        : createSecretStore(name.trim(), body);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: secretStoresKey });
      navigate("/vault");
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const errs: Record<string, string> = {};
    const n = name.trim();
    if (
      !nameRe.test(n) ||
      n.length > 241 ||
      n.endsWith("-credentials") ||
      n.endsWith("-env")
    ) {
      errs.name =
        "Lowercase letters, digits, and hyphens; -credentials and -env endings are reserved.";
    }
    if (!isHTTPSURL(server.trim())) {
      errs.server =
        "Must be an https:// URL — the store credential travels over this connection.";
    }
    if (path.trim() === "") {
      errs.path = "The KV secrets-engine mount path, for example: secret.";
    }
    if (!edit && token === "") {
      errs.token = "A Vault token is required to connect.";
    }
    setErrors(errs);
    if (Object.keys(errs).length === 0) {
      save.mutate();
    }
  };

  return (
    <section className="flex max-w-xl flex-col gap-6">
      <h1 className="font-display text-2xl font-medium tracking-tight text-white">
        {edit ? `Rotate ${edit}` : "Connect a secret store"}
      </h1>
      <p className="text-muted-foreground text-sm">
        {edit
          ? "Update the store's endpoint or rotate its token. Leaving the token empty keeps the current credential."
          : "HashiCorp Vault over KV. The token is stored write-only in a Kubernetes Secret — Orkano can rotate it but never read it back. Scope it to the paths Orkano should reach."}
      </p>
      <form className="flex flex-col gap-6" onSubmit={submit}>
        <ApiErrorAlert error={save.error} />
        <Card>
          <CardContent className="flex flex-col gap-4">
            {!edit && (
              <Field
                id="store-name"
                label="Name"
                error={errors.name}
                hint="How apps and the doctor refer to this store."
              >
                <Input
                  id="store-name"
                  value={name}
                  onChange={(e) => {
                    setName(e.target.value);
                  }}
                  autoFocus
                  required
                />
              </Field>
            )}
            <Field
              id="store-server"
              label="Vault server"
              error={errors.server}
              hint="https://vault.example.com:8200"
            >
              <Input
                id="store-server"
                value={server}
                onChange={(e) => {
                  setServer(e.target.value);
                }}
                required
              />
            </Field>
            <Field
              id="store-path"
              label="Mount path"
              error={errors.path}
              hint="The KV secrets engine's mount, usually: secret"
            >
              <Input
                id="store-path"
                value={path}
                onChange={(e) => {
                  setPath(e.target.value);
                }}
                required
              />
            </Field>
            <Field id="store-version" label="KV engine version">
              <Select
                id="store-version"
                value={version}
                onChange={(e) => {
                  setVersion(e.target.value);
                }}
              >
                <option value="v2">KV v2 (versioned, the default)</option>
                <option value="v1">KV v1</option>
              </Select>
            </Field>
            <Field
              id="store-token"
              label={edit ? "New token (optional)" : "Vault token"}
              error={errors.token}
              hint="Write-only; never displayed again."
            >
              <Input
                id="store-token"
                type="password"
                autoComplete="off"
                value={token}
                onChange={(e) => {
                  setToken(e.target.value);
                }}
                required={!edit}
              />
            </Field>
          </CardContent>
        </Card>
        <div className="flex gap-3">
          <Button type="submit" disabled={save.isPending}>
            {save.isPending
              ? "Saving…"
              : edit
                ? "Save changes"
                : "Connect store"}
          </Button>
          <Button asChild variant="ghost">
            <Link to="/vault">Cancel</Link>
          </Button>
        </div>
      </form>
      <StepUpGate
        error={save.error}
        onConfirmed={() => {
          save.mutate();
        }}
        onDismiss={() => {
          save.reset();
        }}
      />
    </section>
  );
}
