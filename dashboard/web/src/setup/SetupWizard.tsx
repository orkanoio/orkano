import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent, type ReactNode } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Field } from "@/components/Field";
import { StatusDot } from "@/components/StatusBadge";
import { StepUpGate } from "@/components/StepUpGate";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { Select } from "@/components/ui/select";
import {
  configureOIDC,
  fetchSetupStatus,
  setAccessMode,
  setupStatusKey,
  startGitHubManifest,
  type AccessMode,
  type SetupCheck,
  type SetupStatus,
} from "@/lib/api";
import { Link } from "@/lib/router";
import { isStepUpRequired } from "@/lib/errors";

import { postManifestToGitHub } from "./github";
import { blockerLabel, setupErrorMessage } from "./messages";

// The rollout commands the wizard surfaces after a connect: both components
// read their credentials at startup, and the dashboard deliberately holds no
// deployments grant to restart them itself (the M2.6 decision).
const receiverRollout =
  "kubectl -n orkano-system rollout restart deployment/orkano-receiver";
const dashboardRollout =
  "kubectl -n orkano-system rollout restart deployment/orkano-dashboard";

const accessModeOptions: { value: AccessMode; label: string; hint: string }[] =
  [
    {
      value: "proxy",
      label: "Port-forward / orkano proxy",
      hint: "Reach the dashboard over kubectl port-forward (or the orkano proxy command). Nothing is exposed; access requires cluster credentials.",
    },
    {
      value: "tailscale",
      label: "Tailscale",
      hint: "Publish the dashboard Service on your tailnet. Access requires tailnet membership.",
    },
    {
      value: "iap",
      label: "Identity-aware proxy",
      hint: "Front the dashboard with an identity-aware proxy (Cloudflare Access, oauth2-proxy, …) that authenticates before traffic reaches it.",
    },
    {
      value: "public",
      label: "Public with enforced SSO",
      hint: "Expose the dashboard on the internet. Do this only with single sign-on connected; every account also carries a second factor.",
    },
  ];

// SetupWizard walks the unmet setup checks in the order the server returns
// them (dependency order — the wizard face of the shared check registry).
export function SetupWizard() {
  const status = useQuery({
    queryKey: setupStatusKey,
    queryFn: fetchSetupStatus,
  });

  if (status.isPending) {
    return <p className="font-mono text-xs text-muted-foreground">Loading setup…</p>;
  }
  if (status.error) {
    return <ApiErrorAlert error={status.error} />;
  }
  const s = status.data;

  return (
    <section className="flex max-w-2xl flex-col gap-6">
      <div className="flex flex-col gap-1">
        <h1 className="font-display text-2xl font-medium tracking-tight text-white">
          Setup
        </h1>
        <p className="text-muted-foreground text-sm">
          Walk the steps below to finish this install. Everything can be
          revisited later.
        </p>
      </div>
      <AccessModeStep status={s} />
      <SignInStep status={s} />
      <GitHubStep status={s} />
      <VaultStep status={s} />
      <DomainStep status={s} />
      <RegistryStep />
    </section>
  );
}

function checkById(s: SetupStatus, id: string): SetupCheck | undefined {
  return s.checks.find((c) => c.id === id);
}

// OutcomeBadge renders a check outcome. "skip" reads as "not applicable" and
// "blocked" points at the unmet prerequisite (as human copy, not a check ID)
// instead of alarming. The message rides both title (hover) and aria-label —
// a title on a non-interactive span never reaches a screen reader.
function OutcomeBadge({ check }: { check: SetupCheck | undefined }) {
  if (!check) {
    return null;
  }
  const render = (
    label: string,
    variant?: "success" | "warning" | "secondary" | "outline" | "destructive",
  ) => (
    <Badge
      variant={variant}
      title={check.message}
      aria-label={check.message ? `${label}: ${check.message}` : label}
    >
      <StatusDot />
      {label}
    </Badge>
  );
  switch (check.outcome) {
    case "pass":
      return render("Done", "success");
    case "skip":
      return render("Not applicable", "secondary");
    case "blocked":
      return render(
        `Waiting on ${blockerLabel(check.blockers?.[0])}`,
        "warning",
      );
    case "error":
      return render("Could not check", "destructive");
    default:
      return render("To do", "secondary");
  }
}

function StepCard({
  title,
  description,
  badge,
  children,
}: {
  title: string;
  description: string;
  badge: ReactNode;
  children?: ReactNode;
}) {
  // The landing's mono-teal step-index look: split a leading "N." off the
  // title for styling only — the rendered text (and so the accessible name
  // the tests query) stays byte-identical. NOT aria-hidden: hiding the index
  // would drop it from the heading's accessible name.
  const numbered = /^(\d+\.)\s(.*)$/.exec(title);
  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between gap-3">
          <CardTitle
            role="heading"
            aria-level={2}
            className="flex items-baseline gap-2 font-display text-lg tracking-tight text-white"
          >
            {numbered ? (
              <>
                <span className="font-mono text-[11px] font-normal tracking-[0.2em] text-primary">
                  {numbered[1]}
                </span>{" "}
                {numbered[2]}
              </>
            ) : (
              title
            )}
          </CardTitle>
          {badge}
        </div>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      {children !== undefined && (
        <CardContent className="flex flex-col gap-3">{children}</CardContent>
      )}
    </Card>
  );
}

// CommandLine shows a copyable shell command the admin must run outside the
// dashboard (the wizard's rollout prompts).
function CommandLine({ command }: { command: string }) {
  return (
    <div className="flex items-center gap-2">
      <code className="bg-terminal text-foreground flex-1 overflow-x-auto rounded-lg border px-3 py-2 font-mono text-xs">
        {command}
      </code>
      <Button
        type="button"
        variant="outline"
        size="sm"
        // The command in the accessible name: several Copy buttons can share a
        // screen, and a bare "Copy" tells a screen-reader user nothing.
        aria-label={`Copy ${command}`}
        onClick={() => {
          void navigator.clipboard.writeText(command).catch(() => {
            // Clipboard access can be denied; the command stays selectable.
          });
        }}
      >
        Copy
      </Button>
    </div>
  );
}

// --- step 1: access mode ---

function AccessModeStep({ status }: { status: SetupStatus }) {
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<AccessMode>(
    // Validated, not cast: an unexpected server value falls back to the default.
    accessModeOptions.find((o) => o.value === status.accessMode)?.value ??
      "proxy",
  );
  const save = useMutation({
    mutationFn: () => setAccessMode(mode),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: setupStatusKey }),
  });
  const selected = accessModeOptions.find((o) => o.value === mode);
  const publicWithoutSSO = mode === "public" && !status.oidcEnabled;

  return (
    <StepCard
      title="1. Access mode"
      description="How this dashboard is reached. It ships ClusterIP-only — nothing is exposed until you set a path up; recording the choice keeps the hardening checks honest."
      badge={<OutcomeBadge check={checkById(status, "setup.access-mode-chosen")} />}
    >
      <ApiErrorAlert error={save.error} formatMessage={setupErrorMessage} />
      <Field id="access-mode" label="Access mode" hint={selected?.hint}>
        <Select
          id="access-mode"
          value={mode}
          onChange={(e) => {
            setMode(e.target.value as AccessMode);
          }}
        >
          {accessModeOptions.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </Select>
      </Field>
      {publicWithoutSSO && (
        <Alert variant="destructive">
          <AlertDescription>
            Public exposure without single sign-on leaves only the local admin
            account between the internet and this cluster. Connect an identity
            provider first (step 2).
          </AlertDescription>
        </Alert>
      )}
      <div>
        <Button
          type="button"
          disabled={save.isPending}
          onClick={() => {
            save.mutate();
          }}
        >
          {save.isPending
            ? "Saving…"
            : status.accessMode
              ? "Update choice"
              : "Save choice"}
        </Button>
      </div>
    </StepCard>
  );
}

// --- step 2: admin + SSO ---

function SignInStep({ status }: { status: SetupStatus }) {
  const [reconfigure, setReconfigure] = useState(false);
  const oidcCheck = checkById(status, "auth.oidc-configured");

  // Close a finished reconfigure: once the rotation is live (enabled, no
  // restart pending), fall back to the "SSO active" state instead of leaving
  // an empty connect form behind. Deps exclude `reconfigure` on purpose — the
  // effect fires on status transitions, never on the user opening the form.
  useEffect(() => {
    if (status.oidcEnabled && !status.oidcPendingRestart) {
      setReconfigure(false);
    }
  }, [status.oidcEnabled, status.oidcPendingRestart]);

  return (
    <StepCard
      title="2. Sign-in"
      description="The local admin (created at bootstrap, with a required second factor) is the break-glass account. Connecting an identity provider is the recommended way in for daily use."
      badge={<OutcomeBadge check={oidcCheck} />}
    >
      {status.oidcPendingRestart ? (
        <>
          <p className="text-muted-foreground text-sm">
            {status.oidcEnabled
              ? "An updated identity-provider configuration is written. Restart the dashboard to apply it:"
              : "The identity-provider configuration is written. Restart the dashboard to activate it:"}
          </p>
          <CommandLine command={dashboardRollout} />
        </>
      ) : status.oidcEnabled && !reconfigure ? (
        <>
          <p className="text-muted-foreground text-sm">
            Single sign-on is active — the SSO button shows on the sign-in
            screen. The local admin keeps working as break-glass.
          </p>
          <div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => {
                setReconfigure(true);
              }}
            >
              Reconfigure
            </Button>
          </div>
        </>
      ) : (
        <OIDCConnectForm status={status} />
      )}
    </StepCard>
  );
}

function OIDCConnectForm({ status }: { status: SetupStatus }) {
  const queryClient = useQueryClient();
  const [issuer, setIssuer] = useState("");
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [allowedEmails, setAllowedEmails] = useState("");
  const [allowedGroups, setAllowedGroups] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});

  // The exact redirect URL the server will register in the Secret — shown up
  // front because the IdP client must be created with it before connecting.
  // The SERVER-authoritative value when ORKANO_PUBLIC_URL pins it (an admin on
  // a port-forward must not register their localhost origin); the local origin
  // only when the server would derive it from this very request anyway.
  const hostDerived = !status.publicUrlConfigured;
  const redirectUrl =
    status.oidcRedirectUrl ||
    `${window.location.origin}/api/auth/oidc/callback`;

  const connect = useMutation({
    mutationFn: () =>
      configureOIDC({
        issuer: issuer.trim(),
        clientId: clientId.trim(),
        clientSecret,
        allowedEmails: allowedEmails.trim(),
        allowedGroups: allowedGroups.trim(),
      }),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: setupStatusKey }),
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const errs: Record<string, string> = {};
    const iss = issuer.trim();
    if (!/^https?:\/\/.+/.test(iss)) {
      errs.issuer = "Enter the issuer URL, e.g. https://idp.example.com.";
    }
    if (clientId.trim() === "") {
      errs.clientId = "Required.";
    }
    if (clientSecret === "") {
      errs.clientSecret = "Required.";
    }
    if (allowedEmails.trim() === "" && allowedGroups.trim() === "") {
      errs.allowedEmails =
        "List at least one email or one group — without an allowlist, sign-in stays disabled.";
    }
    setErrors(errs);
    if (Object.keys(errs).length === 0) {
      connect.mutate();
    }
  };

  // The established 403 handler, derived purely from the mutation error (no
  // parallel open/closed state): step_up_required swaps the whole view — the
  // gate cannot nest INSIDE the form below, because StepUpForm is itself a
  // <form> and HTML collapses nested forms, double-firing the outer submit.
  if (isStepUpRequired(connect.error)) {
    return (
      <StepUpGate
        error={connect.error}
        onConfirmed={() => {
          connect.mutate();
        }}
        onDismiss={() => {
          connect.reset();
        }}
      />
    );
  }

  return (
    <form className="flex flex-col gap-4" onSubmit={submit}>
      <ApiErrorAlert error={connect.error} formatMessage={setupErrorMessage} />
      <div className="flex flex-col gap-1">
        <p className="text-sm">
          Register a client at your identity provider with this redirect URL:
        </p>
        <CommandLine command={redirectUrl} />
        {hostDerived && (
          <p className="text-muted-foreground text-xs">
            Derived from how you are reaching the dashboard right now — it is
            written permanently, so make sure sign-ins will arrive the same
            way (or pin a canonical URL via ORKANO_PUBLIC_URL on the dashboard
            Deployment first).
          </p>
        )}
      </div>
      <Field id="oidc-issuer" label="Issuer URL" error={errors.issuer}>
        <Input
          id="oidc-issuer"
          value={issuer}
          onChange={(e) => {
            setIssuer(e.target.value);
          }}
          placeholder="https://idp.example.com"
          required
        />
      </Field>
      <Field id="oidc-client-id" label="Client ID" error={errors.clientId}>
        <Input
          id="oidc-client-id"
          value={clientId}
          onChange={(e) => {
            setClientId(e.target.value);
          }}
          required
        />
      </Field>
      <Field
        id="oidc-client-secret"
        label="Client secret"
        error={errors.clientSecret}
        hint="Stored only as a Kubernetes Secret — the dashboard cannot read it back."
      >
        <Input
          id="oidc-client-secret"
          type="password"
          value={clientSecret}
          onChange={(e) => {
            setClientSecret(e.target.value);
          }}
          required
        />
      </Field>
      <Field
        id="oidc-allowed-emails"
        label="Allowed emails"
        error={errors.allowedEmails}
        hint="Comma-separated. Only listed (verified) emails may sign in."
      >
        <Input
          id="oidc-allowed-emails"
          value={allowedEmails}
          onChange={(e) => {
            setAllowedEmails(e.target.value);
          }}
          placeholder="you@example.com"
        />
      </Field>
      <Field
        id="oidc-allowed-groups"
        label="Allowed groups"
        hint="Comma-separated. Members of any listed group may sign in (needed for IdPs that omit email_verified)."
      >
        <Input
          id="oidc-allowed-groups"
          value={allowedGroups}
          onChange={(e) => {
            setAllowedGroups(e.target.value);
          }}
        />
      </Field>
      <div>
        <Button type="submit" disabled={connect.isPending}>
          {connect.isPending ? "Validating…" : "Connect identity provider"}
        </Button>
      </div>
    </form>
  );
}

// --- step 3: GitHub App ---

function GitHubStep({ status }: { status: SetupStatus }) {
  const check = checkById(status, "github.app-connected");

  return (
    <StepCard
      title="3. GitHub"
      description="One click creates a GitHub App via the manifest flow — exact permissions (repository contents + metadata, read-only), push webhooks pre-wired to this install."
      badge={<OutcomeBadge check={check} />}
    >
      {status.github.connected ? (
        <>
          <p className="text-muted-foreground text-sm">
            Connected{status.github.appSlug ? ` as ${status.github.appSlug}` : ""}
            {status.github.appId ? ` (App ID ${status.github.appId})` : ""}.
            Install the App on the repositories you want to deploy, and add
            them to the receiver allowlist.
          </p>
          {isRecentConnect(status.github.connectedAt) ? (
            <>
              <p className="text-sm">
                The receiver reads the webhook secret at startup — if it was
                running during the connect, restart it once:
              </p>
              <CommandLine command={receiverRollout} />
            </>
          ) : (
            <p className="text-muted-foreground text-sm">
              Push webhooks are delivered to this install.
            </p>
          )}
        </>
      ) : !status.webhookUrlConfigured ? (
        <Alert>
          <AlertDescription>
            The receiver's public webhook URL is not configured, so GitHub
            would have nowhere to deliver push events. Re-run{" "}
            <code className="font-mono text-xs">
              orkano init --receiver-host &lt;host&gt;
            </code>{" "}
            (or set{" "}
            <code className="font-mono text-xs">ORKANO_WEBHOOK_URL</code> on
            the dashboard Deployment), then come back.
          </AlertDescription>
        </Alert>
      ) : (
        <GitHubConnectForm />
      )}
    </StepCard>
  );
}

// isRecentConnect scopes the receiver-restart reminder to the hour after a
// connect: the stale-secret window closes with the first restart, and a
// standing call-to-action on every later wizard visit reads as unmet work.
function isRecentConnect(connectedAt: string | undefined): boolean {
  if (!connectedAt) {
    return false;
  }
  const ts = Date.parse(connectedAt);
  return Number.isFinite(ts) && Date.now() - ts < 60 * 60 * 1000;
}

function GitHubConnectForm() {
  const [org, setOrg] = useState("");
  const start = useMutation({
    mutationFn: () => startGitHubManifest(org.trim() ? { org: org.trim() } : {}),
    onSuccess: postManifestToGitHub,
  });

  return (
    <div className="flex flex-col gap-4">
      <ApiErrorAlert error={start.error} formatMessage={setupErrorMessage} />
      <Field
        id="github-org"
        label="Organization (optional)"
        hint="Leave empty to create the App on your personal account."
      >
        <Input
          id="github-org"
          value={org}
          onChange={(e) => {
            setOrg(e.target.value);
          }}
        />
      </Field>
      <div>
        <Button
          type="button"
          disabled={start.isPending}
          onClick={() => {
            start.mutate();
          }}
        >
          {start.isPending ? "Preparing…" : "Create GitHub App"}
        </Button>
      </div>
      <p className="text-muted-foreground text-xs">
        You will be taken to GitHub to confirm, then brought straight back.
      </p>
    </div>
  );
}

// --- step 4: secrets ---

// VaultStep reports the optional external-vault path (ADR-0018). "Not
// applicable" means the install never opted into the External Secrets
// Operator — the card shows the exact re-run one-liner that adds it.
function VaultStep({ status }: { status: SetupStatus }) {
  const check = checkById(status, "secrets.vault-connected");
  return (
    <StepCard
      title="4. Secrets"
      description="Secrets typed into an app's environment live only as Kubernetes Secrets, encrypted at rest. Optionally, sync them from an external vault instead — Orkano then never holds the values at all."
      badge={<OutcomeBadge check={check} />}
    >
      {check?.outcome === "skip" ? (
        <div className="flex flex-col gap-2">
          <p className="text-muted-foreground text-sm">
            The External Secrets Operator is not installed. To add it, re-run
            the installer with the flag — the re-run converges, it does not
            reinstall:
          </p>
          <pre className="bg-terminal text-foreground overflow-x-auto rounded-lg border p-3 font-mono text-xs">
            orkano init --secrets-vault …your original flags…
          </pre>
        </div>
      ) : (
        <p className="text-muted-foreground text-sm">
          {check?.message ?? ""}{" "}
          <Link to="/vault" className="text-primary hover:underline">
            Manage stores and synced secrets on the Vault page.
          </Link>
        </p>
      )}
    </StepCard>
  );
}

// --- step 5: domain + TLS ---

function DomainStep({ status }: { status: SetupStatus }) {
  const check = checkById(status, "domains.tls-ready");
  return (
    <StepCard
      title="5. Domains & TLS"
      description="Custom domains live on each app: add one from the app's screen, point its DNS at the cluster, and cert-manager issues the certificate through the orkano-platform issuer."
      badge={<OutcomeBadge check={check} />}
    >
      <p className="text-muted-foreground text-sm">
        {check?.message ?? ""}{" "}
        {check?.outcome !== "pass" &&
          "Certificates come from Let's Encrypt staging unless init ran with --acme-prod (staging avoids rate limits while you experiment; browsers will warn until you switch)."}
      </p>
    </StepCard>
  );
}

// --- step 6: registry ---

function RegistryStep() {
  // No server check backs this step — the badge says "nothing to do here",
  // deliberately not "verified healthy" (that is a doctor check, Phase 3).
  return (
    <StepCard
      title="6. Registry"
      description="Builds push to the in-cluster registry deployed at install — TLS from an internal CA, images digest-pinned into every rollout. Nothing to configure in v1; external registries are on the roadmap."
      badge={<Badge variant="secondary">Built in</Badge>}
    />
  );
}
