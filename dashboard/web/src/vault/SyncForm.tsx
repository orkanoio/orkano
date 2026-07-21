import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Field } from "@/components/Field";
import { StepUpGate } from "@/components/StepUpGate";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import {
  createExternalSecret,
  externalSecretsKey,
  listSecretStores,
  secretStoresKey,
  type SyncKey,
} from "@/lib/api";
import { Link, navigate } from "@/lib/router";

const nameRe = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;
// The EnvVar.Name pattern (api/v1alpha1) — the synced Secret's keys become
// env-var names when an app references them.
const keyRe = /^[A-Za-z_][A-Za-z0-9_]*$/;

// SyncForm creates one ExternalSecret: named vault keys materialized as one
// Kubernetes Secret (named after the sync) apps reference from their env.
export function SyncForm() {
  const queryClient = useQueryClient();
  const stores = useQuery({
    queryKey: secretStoresKey,
    queryFn: listSecretStores,
  });
  const [name, setName] = useState("");
  const [storeName, setStoreName] = useState("");
  const [interval, setInterval] = useState("1h");
  const [keys, setKeys] = useState<SyncKey[]>([
    { secretKey: "", remoteKey: "" },
  ]);
  const [errors, setErrors] = useState<Record<string, string>>({});

  const create = useMutation({
    mutationFn: () =>
      createExternalSecret({
        name: name.trim(),
        // The first store is the select's visible default; mirror it when the
        // user never touched the control.
        storeName: storeName || (stores.data?.[0]?.name ?? ""),
        refreshInterval: interval.trim(),
        keys: keys.map((k) => ({
          secretKey: k.secretKey.trim(),
          remoteKey: k.remoteKey.trim(),
        })),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: externalSecretsKey });
      navigate("/vault");
    },
  });

  const setKey = (i: number, patch: Partial<SyncKey>) => {
    setKeys((prev) => prev.map((k, j) => (j === i ? { ...k, ...patch } : k)));
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const errs: Record<string, string> = {};
    const n = name.trim();
    if (
      !nameRe.test(n) ||
      n.length > 253 ||
      n.endsWith("-credentials") ||
      n.endsWith("-env")
    ) {
      errs.name =
        "Lowercase letters, digits, and hyphens; -credentials and -env endings are reserved.";
    }
    const seen = new Set<string>();
    for (const k of keys) {
      const sk = k.secretKey.trim();
      if (!keyRe.test(sk) || seen.has(sk) || k.remoteKey.trim() === "") {
        errs.keys =
          "Each key needs a unique variable name (letters, digits, underscores) and a vault path.";
      }
      seen.add(sk);
    }
    setErrors(errs);
    if (Object.keys(errs).length === 0) {
      create.mutate();
    }
  };

  if (stores.data && stores.data.length === 0) {
    return (
      <section className="flex max-w-xl flex-col gap-2">
        <h1 className="font-display text-2xl font-medium tracking-tight text-white">
          New synced secret
        </h1>
        <p className="text-muted-foreground text-sm">
          A sync needs a connected store first.{" "}
          <Link to="/vault/connect" className="text-primary hover:underline">
            Connect your vault
          </Link>{" "}
          and come back.
        </p>
      </section>
    );
  }

  return (
    <section className="flex max-w-xl flex-col gap-6">
      <h1 className="font-display text-2xl font-medium tracking-tight text-white">
        New synced secret
      </h1>
      <p className="text-muted-foreground text-sm">
        Produces a Kubernetes Secret named after this sync, kept up to date
        from the store. Wire it into an app from the app's environment editor
        by this name and the key names below.
      </p>
      <form className="flex flex-col gap-6" onSubmit={submit}>
        <ApiErrorAlert error={create.error} />
        <Card>
          <CardContent className="flex flex-col gap-4">
            <Field
              id="sync-name"
              label="Name"
              error={errors.name}
              hint="Also the produced Secret's name — what apps reference."
            >
              <Input
                id="sync-name"
                value={name}
                onChange={(e) => {
                  setName(e.target.value);
                }}
                autoFocus
                required
              />
            </Field>
            <Field id="sync-store" label="Store">
              <Select
                id="sync-store"
                value={storeName || (stores.data?.[0]?.name ?? "")}
                onChange={(e) => {
                  setStoreName(e.target.value);
                }}
              >
                {(stores.data ?? []).map((s) => (
                  <option key={s.name} value={s.name}>
                    {s.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Field
              id="sync-interval"
              label="Refresh interval"
              hint="How often the value is re-read from the store."
            >
              <Input
                id="sync-interval"
                value={interval}
                onChange={(e) => {
                  setInterval(e.target.value);
                }}
                required
              />
            </Field>
            {/* Not a <Field>: its aria wiring clones onto a single input child,
                which is inert on this multi-input group — the rows carry their
                own aria-labels and point at the shared error text instead. */}
            <div className="flex flex-col gap-1.5">
              <Label>Keys</Label>
              <div className="flex flex-col gap-2">
                {keys.map((k, i) => (
                  <div key={i} className="flex gap-2">
                    <Input
                      id={`sync-key-${String(i)}`}
                      placeholder="STRIPE_KEY"
                      aria-label={`Variable name ${String(i + 1)}`}
                      aria-invalid={errors.keys ? true : undefined}
                      aria-describedby={errors.keys ? "sync-keys-error" : undefined}
                      value={k.secretKey}
                      onChange={(e) => {
                        setKey(i, { secretKey: e.target.value });
                      }}
                      required
                    />
                    <Input
                      placeholder="orkano/api/stripe"
                      aria-label={`Vault path ${String(i + 1)}`}
                      aria-invalid={errors.keys ? true : undefined}
                      aria-describedby={errors.keys ? "sync-keys-error" : undefined}
                      value={k.remoteKey}
                      onChange={(e) => {
                        setKey(i, { remoteKey: e.target.value });
                      }}
                      required
                    />
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      aria-label={`Remove key ${String(i + 1)}`}
                      disabled={keys.length === 1}
                      onClick={() => {
                        setKeys((prev) => prev.filter((_, j) => j !== i));
                      }}
                    >
                      ✕
                    </Button>
                  </div>
                ))}
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  className="self-start"
                  disabled={keys.length >= 64}
                  onClick={() => {
                    setKeys((prev) => [...prev, { secretKey: "", remoteKey: "" }]);
                  }}
                >
                  Add key
                </Button>
              </div>
              {errors.keys && (
                <p id="sync-keys-error" role="alert" className="text-destructive text-sm">
                  {errors.keys}
                </p>
              )}
            </div>
          </CardContent>
        </Card>
        <div className="flex gap-3">
          <Button
            type="submit"
            disabled={create.isPending || (stores.data ?? []).length === 0}
          >
            {create.isPending ? "Creating…" : "Create sync"}
          </Button>
          <Button asChild variant="ghost">
            <Link to="/vault">Cancel</Link>
          </Button>
        </div>
      </form>
      <StepUpGate
        error={create.error}
        onConfirmed={() => {
          create.mutate();
        }}
        onDismiss={() => {
          create.reset();
        }}
      />
    </section>
  );
}
