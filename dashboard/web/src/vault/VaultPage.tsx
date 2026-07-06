import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { StatusBadge } from "@/components/StatusBadge";
import { StepUpGate } from "@/components/StepUpGate";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  ApiError,
  deleteExternalSecret,
  deleteSecretStore,
  externalSecretsKey,
  listExternalSecrets,
  listSecretStores,
  secretStoresKey,
  type ExternalSecretItem,
  type SecretStoreItem,
} from "@/lib/api";
import { formatAge } from "@/lib/format";
import { Link } from "@/lib/router";

function isNotInstalled(err: unknown): boolean {
  return err instanceof ApiError && err.code === "secrets_vault_not_installed";
}

// readyConditions adapts the vault DTOs' flat ready/reason/message to the
// Condition shape StatusBadge already renders for every other kind.
function readyConditions(item: {
  ready: "True" | "False" | "Unknown";
  reason?: string;
  message?: string;
}) {
  return [
    {
      type: "Ready",
      status: item.ready,
      reason: item.reason,
      message: item.message,
    },
  ];
}

export function VaultPage() {
  const stores = useQuery({
    queryKey: secretStoresKey,
    queryFn: listSecretStores,
    refetchInterval: 10_000,
  });
  const syncs = useQuery({
    queryKey: externalSecretsKey,
    queryFn: listExternalSecrets,
    refetchInterval: 10_000,
  });

  if (isNotInstalled(stores.error) || isNotInstalled(syncs.error)) {
    return <NotInstalled />;
  }

  return (
    <section className="flex flex-col gap-8">
      <div className="flex flex-col gap-4">
        <div className="flex items-center justify-between">
          <h1 className="text-xl font-semibold">Secret stores</h1>
          <Button asChild>
            <Link to="/vault/connect">Connect a store</Link>
          </Button>
        </div>
        <p className="text-muted-foreground text-sm">
          External vaults your secrets live in. Orkano never stores the values
          — the External Secrets Operator syncs them into ordinary Kubernetes
          Secrets your apps reference by name.
        </p>
        {stores.isPending && (
          <p className="text-muted-foreground text-sm">Loading…</p>
        )}
        <ApiErrorAlert error={stores.error} />
        {stores.data && <StoresTable stores={stores.data} />}
      </div>

      <div className="flex flex-col gap-4">
        <div className="flex items-center justify-between">
          <h2 className="text-xl font-semibold">Synced secrets</h2>
          {stores.data && stores.data.length > 0 ? (
            <Button asChild>
              <Link to="/vault/sync">New sync</Link>
            </Button>
          ) : (
            // A real disabled <button>, not asChild: disabled never reaches
            // (or styles) an <a>, so the Link form would stay clickable.
            <Button disabled>New sync</Button>
          )}
        </div>
        <p className="text-muted-foreground text-sm">
          Each sync materializes one Kubernetes Secret from vault keys.
          Reference it from an app's environment by the sync's name.
        </p>
        {syncs.isPending && (
          <p className="text-muted-foreground text-sm">Loading…</p>
        )}
        <ApiErrorAlert error={syncs.error} />
        {syncs.data && <SyncsTable syncs={syncs.data} />}
      </div>
    </section>
  );
}

function NotInstalled() {
  return (
    <section className="flex max-w-xl flex-col gap-4">
      <h1 className="text-xl font-semibold">Secret stores</h1>
      <p className="text-muted-foreground text-sm">
        External vault support is opt-in and not installed on this cluster.
        Re-run the installer with the flag to add the External Secrets
        Operator — the re-run converges, it does not reinstall:
      </p>
      <pre className="bg-muted overflow-x-auto rounded-md p-3 font-mono text-xs">
        orkano init --secrets-vault …your original flags…
      </pre>
      <p className="text-muted-foreground text-sm">
        Until then, secrets typed into an app's environment editor are stored
        as Kubernetes Secrets, encrypted at rest.
      </p>
    </section>
  );
}

function StoresTable({ stores }: { stores: SecretStoreItem[] }) {
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState<string | null>(null);
  const del = useMutation({
    mutationFn: deleteSecretStore,
    onSuccess: () => {
      setConfirming(null);
      void queryClient.invalidateQueries({ queryKey: secretStoresKey });
    },
  });

  if (stores.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        No stores connected yet — connect your vault to sync secrets from it.
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-2">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Provider</TableHead>
            <TableHead>Server</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Age</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {stores.map((store) => (
            <TableRow key={store.name}>
              <TableCell className="font-medium">{store.name}</TableCell>
              <TableCell>{store.provider}</TableCell>
              <TableCell className="max-w-56 truncate" title={store.server}>
                {store.server ?? "—"}
              </TableCell>
              <TableCell>
                <StatusBadge conditions={readyConditions(store)} />
              </TableCell>
              <TableCell>{formatAge(store.creationTimestamp)}</TableCell>
              <TableCell className="text-right">
                <div className="flex justify-end gap-2">
                  <Button asChild variant="ghost" size="sm">
                    <Link to={`/vault/connect/${encodeURIComponent(store.name)}`}>
                      Rotate
                    </Link>
                  </Button>
                  {confirming === store.name ? (
                    <>
                      <Button
                        variant="destructive"
                        size="sm"
                        disabled={del.isPending}
                        onClick={() => {
                          del.mutate(store.name);
                        }}
                      >
                        Really disconnect
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => {
                          setConfirming(null);
                          del.reset();
                        }}
                      >
                        Keep
                      </Button>
                    </>
                  ) : (
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => {
                        setConfirming(store.name);
                      }}
                    >
                      Disconnect
                    </Button>
                  )}
                </div>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <ApiErrorAlert error={del.error} />
      <StepUpGate
        error={del.error}
        onConfirmed={() => {
          if (del.variables !== undefined) {
            del.mutate(del.variables);
          }
        }}
        onDismiss={() => {
          del.reset();
        }}
      />
    </div>
  );
}

function SyncsTable({ syncs }: { syncs: ExternalSecretItem[] }) {
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState<string | null>(null);
  const del = useMutation({
    mutationFn: deleteExternalSecret,
    onSuccess: () => {
      setConfirming(null);
      void queryClient.invalidateQueries({ queryKey: externalSecretsKey });
    },
  });

  if (syncs.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        No synced secrets yet — a sync pulls named keys out of a connected
        store on a schedule.
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-2">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Secret</TableHead>
            <TableHead>Store</TableHead>
            <TableHead>Keys</TableHead>
            <TableHead>Refresh</TableHead>
            <TableHead>Status</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {syncs.map((sync) => (
            <TableRow key={sync.name}>
              <TableCell className="font-medium">{sync.name}</TableCell>
              <TableCell>{sync.storeName}</TableCell>
              <TableCell
                className="max-w-56 truncate"
                title={sync.keys.map((k) => k.secretKey).join(", ")}
              >
                {sync.keys.map((k) => k.secretKey).join(", ") || "—"}
              </TableCell>
              <TableCell>{sync.refreshInterval ?? "1h"}</TableCell>
              <TableCell>
                <StatusBadge conditions={readyConditions(sync)} />
              </TableCell>
              <TableCell className="text-right">
                {confirming === sync.name ? (
                  <div className="flex justify-end gap-2">
                    <Button
                      variant="destructive"
                      size="sm"
                      disabled={del.isPending}
                      onClick={() => {
                        del.mutate(sync.name);
                      }}
                    >
                      Really remove
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => {
                        setConfirming(null);
                        del.reset();
                      }}
                    >
                      Keep
                    </Button>
                  </div>
                ) : (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => {
                      setConfirming(sync.name);
                    }}
                  >
                    Remove
                  </Button>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <ApiErrorAlert error={del.error} />
      <StepUpGate
        error={del.error}
        onConfirmed={() => {
          if (del.variables !== undefined) {
            del.mutate(del.variables);
          }
        }}
        onDismiss={() => {
          del.reset();
        }}
      />
    </div>
  );
}
