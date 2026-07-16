import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { ConditionBadge, StatusBadge } from "@/components/StatusBadge";
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
import {
  createDomain,
  deleteDomain,
  domainsKey,
  listDomains,
} from "@/lib/api";

// hostRe mirrors the Domain host pattern (api/v1alpha1): lowercase DNS labels,
// at least two of them.
const hostRe =
  /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$/;

// DomainsCard manages the Domains pointing at one App. Domain spec is fully
// immutable (ADR-0006) so there is no edit — only add and (step-up-gated)
// remove; the Domain object is named by its host.
export function DomainsCard({ appName }: { appName: string }) {
  const queryClient = useQueryClient();
  const query = useQuery({
    queryKey: domainsKey,
    queryFn: listDomains,
    refetchInterval: 10_000,
  });
  const [host, setHost] = useState("");
  const [hostError, setHostError] = useState("");
  // The host whose removal is awaiting the explicit confirm click — removal
  // takes down live routing + the certificate, so one misclick must not do it.
  const [confirmingRemove, setConfirmingRemove] = useState<string | null>(null);

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: domainsKey });

  const add = useMutation({
    mutationFn: (h: string) =>
      // The object is named by the host — a valid host is a valid (DNS-1123)
      // object name, and one hostname is one Domain.
      createDomain(h, { host: h, appRef: { name: appName } }),
    onSuccess: () => {
      setHost("");
      void invalidate();
    },
  });

  const remove = useMutation({
    mutationFn: deleteDomain,
    onSuccess: () => {
      setConfirmingRemove(null);
      void invalidate();
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const h = host.trim().toLowerCase();
    // 249, not 253: the Domain root CEL caps the object name (= this host) so
    // the derived "<name>-tls" Secret name fits.
    if (h.length > 249 || !hostRe.test(h)) {
      setHostError("Enter a full hostname, e.g. app.example.com.");
      return;
    }
    setHostError("");
    add.mutate(h);
  };

  const domains = (query.data ?? []).filter(
    (d) => d.spec.appRef.name === appName,
  );

  return (
    <Card>
      <CardHeader>
        <CardTitle>Domains</CardTitle>
        <CardDescription>
          Hostnames routed to this app, each with a Let's Encrypt certificate.
          A hostname cannot be edited — remove it and add the new one.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <ApiErrorAlert error={query.error} />
        {query.data && domains.length === 0 && (
          <p className="border-primary/50 text-primary rounded-lg border border-dashed px-5 py-4 font-mono text-[13px] leading-relaxed">
            No domains.
          </p>
        )}
        {domains.map((d) => (
          <div key={d.name} className="flex flex-wrap items-center gap-2">
            <span className="text-foreground font-mono text-[13px]">
              {d.spec.host}
            </span>
            <StatusBadge conditions={d.status.conditions} />
            <ConditionBadge
              conditions={d.status.conditions}
              type="CertificateReady"
              label="certificate"
            />
            {confirmingRemove === d.name ? (
              <>
                <span className="text-muted-foreground text-sm">
                  Remove this hostname and its certificate?
                </span>
                <Button
                  type="button"
                  variant="destructive"
                  size="sm"
                  disabled={remove.isPending}
                  onClick={() => {
                    remove.mutate(d.name);
                  }}
                >
                  {remove.isPending ? "Removing…" : "Confirm remove"}
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    setConfirmingRemove(null);
                    remove.reset();
                  }}
                >
                  Cancel
                </Button>
              </>
            ) : (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => {
                  setConfirmingRemove(d.name);
                }}
              >
                Remove
              </Button>
            )}
          </div>
        ))}
        <ApiErrorAlert error={remove.error} />
        <StepUpGate
          error={remove.error}
          onConfirmed={() => {
            if (remove.variables !== undefined) {
              remove.mutate(remove.variables);
            }
          }}
          onDismiss={() => {
            remove.reset();
          }}
        />
        <form className="flex flex-wrap items-start gap-2" onSubmit={submit}>
          <div className="flex flex-col gap-1">
            <Input
              aria-label="Hostname"
              className="w-64"
              placeholder="app.example.com"
              value={host}
              onChange={(e) => {
                setHost(e.target.value);
              }}
            />
            {hostError !== "" && (
              <p className="text-destructive text-xs">{hostError}</p>
            )}
          </div>
          <Button type="submit" size="sm" disabled={add.isPending}>
            {add.isPending ? "Adding…" : "Add domain"}
          </Button>
        </form>
        <ApiErrorAlert error={add.error} />
      </CardContent>
    </Card>
  );
}
