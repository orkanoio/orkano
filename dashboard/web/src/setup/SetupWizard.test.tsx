import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { setupStatusKey, type SetupCheck, type SetupStatus } from "@/lib/api";
import { SetupWizard } from "@/setup/SetupWizard";
import { SessionContext } from "@/shell/session";
import {
  emptyResponse,
  jsonResponse,
  renderWithSession,
  requestBody,
  stubFetchRoutes,
} from "@/test/helpers";

// A brand-new install as handleSetupStatus reports it: nothing configured,
// GitHub blocked behind the missing webhook URL, domains not applicable.
const freshChecks: SetupCheck[] = [
  {
    id: "setup.access-mode-chosen",
    severity: "warning",
    outcome: "fail",
    message: "no access mode chosen yet",
  },
  { id: "auth.admin-bootstrapped", severity: "critical", outcome: "pass" },
  {
    id: "auth.oidc-configured",
    severity: "info",
    outcome: "fail",
    message: "no identity provider connected (optional)",
  },
  {
    id: "github.webhook-url-configured",
    severity: "critical",
    outcome: "fail",
  },
  {
    id: "github.app-connected",
    severity: "critical",
    outcome: "blocked",
    blockers: ["github.webhook-url-configured"],
  },
  {
    id: "secrets.vault-connected",
    severity: "info",
    outcome: "skip",
    message: "External Secrets Operator not installed — optional",
  },
  {
    id: "domains.tls-ready",
    severity: "info",
    outcome: "skip",
    message: "no Domains yet — not applicable until an app has a custom domain",
  },
];

function makeStatus(over: Partial<SetupStatus> = {}): SetupStatus {
  return {
    checks: freshChecks,
    accessMode: "",
    webhookUrlConfigured: false,
    publicUrlConfigured: false,
    oidcRedirectUrl: "",
    oidcEnabled: false,
    oidcPendingRestart: false,
    github: { connected: false },
    repoAllowlist: [],
    ...over,
  };
}

function statusRoute(status: SetupStatus) {
  return { "GET /api/setup/status": () => jsonResponse(200, status) };
}

describe("SetupWizard", () => {
  it("renders the six steps with the fresh-install outcomes", async () => {
    stubFetchRoutes(statusRoute(makeStatus()));
    renderWithSession(<SetupWizard />);

    expect(
      await screen.findByRole("heading", { name: "1. Access mode" }),
    ).toBeInTheDocument();
    for (const name of [
      "2. Sign-in",
      "3. GitHub",
      "4. Secrets",
      "5. Domains & TLS",
      "6. Registry",
    ]) {
      expect(screen.getByRole("heading", { name })).toBeInTheDocument();
    }
    // The blocked GitHub step names its prerequisite as human copy (never the
    // raw check ID), and the webhook-URL guidance shows in its body.
    expect(
      screen.getByText("Waiting on the webhook URL"),
    ).toBeInTheDocument();
    expect(screen.getByText(/--receiver-host/)).toBeInTheDocument();
    // Two optional skips on a fresh install: the vault and domains steps.
    expect(screen.getAllByText("Not applicable")).toHaveLength(2);
    // The vault skip shows the exact opt-in re-run one-liner.
    expect(screen.getByText(/orkano init --secrets-vault/)).toBeInTheDocument();
    expect(screen.getByText("Built in")).toBeInTheDocument(); // registry: no server check backs it
  });

  it("links the vault page when a store is connected but unhealthy", async () => {
    stubFetchRoutes(
      statusRoute(
        makeStatus({
          checks: makeStatus().checks.map((c) =>
            c.id === "secrets.vault-connected"
              ? {
                  ...c,
                  outcome: "fail",
                  message: "1 store(s) connected, none Ready",
                }
              : c,
          ),
        }),
      ),
    );
    renderWithSession(<SetupWizard />);

    expect(
      await screen.findByRole("link", {
        name: /Manage stores and synced secrets/,
      }),
    ).toHaveAttribute("href", "#/vault");
    expect(screen.getByText(/none Ready/)).toBeInTheDocument();
    expect(
      screen.queryByText(/orkano init --secrets-vault/),
    ).not.toBeInTheDocument();
  });

  it("saves the access mode and refreshes the status", async () => {
    let statusCalls = 0;
    const mock = stubFetchRoutes({
      "GET /api/setup/status": () => {
        statusCalls++;
        return jsonResponse(200, makeStatus());
      },
      "POST /api/setup/access-mode": () => emptyResponse(204),
    });
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.selectOptions(
      await screen.findByLabelText("Access mode"),
      "tailscale",
    );
    await user.click(screen.getByRole("button", { name: "Save choice" }));

    expect(await requestBody(mock, 1)).toEqual({ mode: "tailscale" });
    // A successful save invalidates the status query.
    await waitFor(() => {
      expect(statusCalls).toBe(2);
    });
  });

  it("labels the save button as an update when a mode was already chosen", async () => {
    stubFetchRoutes(statusRoute(makeStatus({ accessMode: "proxy" })));
    renderWithSession(<SetupWizard />);

    expect(
      await screen.findByRole("button", { name: "Update choice" }),
    ).toBeInTheDocument();
  });

  it("warns when choosing public exposure without SSO", async () => {
    stubFetchRoutes(statusRoute(makeStatus()));
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.selectOptions(
      await screen.findByLabelText("Access mode"),
      "public",
    );
    expect(
      screen.getByText(/Public exposure without single sign-on/),
    ).toBeInTheDocument();
  });

  it("refuses an OIDC form without any allowlist entry, before any request", async () => {
    stubFetchRoutes(statusRoute(makeStatus()));
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.type(
      await screen.findByLabelText("Issuer URL"),
      "https://idp.example.com",
    );
    await user.type(screen.getByLabelText("Client ID"), "orkano");
    await user.type(screen.getByLabelText("Client secret"), "s3cret");
    await user.click(
      screen.getByRole("button", { name: "Connect identity provider" }),
    );

    expect(
      await screen.findByText(/List at least one email or one group/),
    ).toBeInTheDocument();
  });

  it("connects an identity provider and refreshes the status", async () => {
    let statusCalls = 0;
    const mock = stubFetchRoutes({
      "GET /api/setup/status": () => {
        statusCalls++;
        return jsonResponse(
          200,
          statusCalls === 1
            ? makeStatus()
            : makeStatus({ oidcPendingRestart: true }),
        );
      },
      "POST /api/setup/oidc": () =>
        jsonResponse(200, {
          redirectUrl: "http://localhost/api/auth/oidc/callback",
          restartRequired: true,
        }),
    });
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.type(
      await screen.findByLabelText("Issuer URL"),
      "https://idp.example.com",
    );
    await user.type(screen.getByLabelText("Client ID"), "orkano");
    await user.type(screen.getByLabelText("Client secret"), "s3cret");
    await user.type(
      screen.getByLabelText("Allowed emails"),
      "ops@example.com",
    );
    await user.click(
      screen.getByRole("button", { name: "Connect identity provider" }),
    );

    expect(await requestBody(mock, 1)).toEqual({
      issuer: "https://idp.example.com",
      clientId: "orkano",
      clientSecret: "s3cret",
      allowedEmails: "ops@example.com",
      allowedGroups: "",
    });
    // The refreshed status flips the step to the restart prompt.
    expect(
      await screen.findByText(
        /rollout restart deployment\/orkano-dashboard/,
      ),
    ).toBeInTheDocument();
  });

  it("opens the step-up form on 403 and retries after confirmation", async () => {
    let oidcCalls = 0;
    stubFetchRoutes({
      ...statusRoute(makeStatus()),
      "POST /api/setup/oidc": () => {
        oidcCalls++;
        return oidcCalls === 1
          ? jsonResponse(403, { error: "step_up_required" })
          : jsonResponse(200, {
              redirectUrl: "http://localhost/api/auth/oidc/callback",
              restartRequired: true,
            });
      },
      "POST /api/auth/stepup": () => emptyResponse(204),
    });
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.type(
      await screen.findByLabelText("Issuer URL"),
      "https://idp.example.com",
    );
    await user.type(screen.getByLabelText("Client ID"), "orkano");
    await user.type(screen.getByLabelText("Client secret"), "s3cret");
    await user.type(
      screen.getByLabelText("Allowed emails"),
      "ops@example.com",
    );
    await user.click(
      screen.getByRole("button", { name: "Connect identity provider" }),
    );

    // The 403 swaps in the step-up form (local admin: password + TOTP).
    await user.type(await screen.findByLabelText("Password"), "hunter2hunter2");
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm identity" }));

    await waitFor(() => {
      expect(oidcCalls).toBe(2);
    });
  });

  it("surfaces an incomplete configuration with its own copy", async () => {
    stubFetchRoutes({
      ...statusRoute(makeStatus()),
      "POST /api/setup/oidc": () =>
        jsonResponse(422, { error: "invalid_oidc_config" }),
    });
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.type(
      await screen.findByLabelText("Issuer URL"),
      "https://idp.example.com",
    );
    await user.type(screen.getByLabelText("Client ID"), "orkano");
    await user.type(screen.getByLabelText("Client secret"), "s3cret");
    await user.type(
      screen.getByLabelText("Allowed emails"),
      "ops@example.com",
    );
    await user.click(
      screen.getByRole("button", { name: "Connect identity provider" }),
    );

    expect(
      await screen.findByText(/configuration is incomplete/),
    ).toBeInTheDocument();
  });

  it("resets a finished reconfigure back to the active state", async () => {
    // Rotation life cycle: enabled (user opens Reconfigure) → submit →
    // pendingRestart → restart happens, a refetch clears the flag — the step
    // must land back on "SSO active", not an empty connect form.
    let statusCalls = 0;
    stubFetchRoutes({
      "GET /api/setup/status": () => {
        statusCalls++;
        if (statusCalls === 1) {
          return jsonResponse(200, makeStatus({ oidcEnabled: true }));
        }
        if (statusCalls === 2) {
          return jsonResponse(
            200,
            makeStatus({ oidcEnabled: true, oidcPendingRestart: true }),
          );
        }
        return jsonResponse(200, makeStatus({ oidcEnabled: true }));
      },
      "POST /api/setup/oidc": () =>
        jsonResponse(200, {
          redirectUrl: "http://localhost/api/auth/oidc/callback",
          restartRequired: true,
        }),
    });
    // A hand-built QueryClient so the test can force the post-restart refetch
    // without remounting (renderWithSession hides its client).
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <SessionContext.Provider value={{ username: "admin", oidc: false }}>
          <SetupWizard />
        </SessionContext.Provider>
      </QueryClientProvider>,
    );
    const user = userEvent.setup();

    await user.click(
      await screen.findByRole("button", { name: "Reconfigure" }),
    );
    await user.type(screen.getByLabelText("Issuer URL"), "https://idp.example.com");
    await user.type(screen.getByLabelText("Client ID"), "orkano");
    await user.type(screen.getByLabelText("Client secret"), "s3cret");
    await user.type(screen.getByLabelText("Allowed emails"), "ops@example.com");
    await user.click(
      screen.getByRole("button", { name: "Connect identity provider" }),
    );
    expect(
      await screen.findByText(/Restart the dashboard to apply it/),
    ).toBeInTheDocument();

    // The rollout happened; the next refetch clears the pending flag and the
    // step must land on "SSO active", never an empty connect form.
    await client.invalidateQueries({ queryKey: setupStatusKey });
    expect(
      await screen.findByText(/Single sign-on is active/),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Issuer URL")).not.toBeInTheDocument();
  });

  it("surfaces a failed discovery with its own copy", async () => {
    stubFetchRoutes({
      ...statusRoute(makeStatus()),
      "POST /api/setup/oidc": () =>
        jsonResponse(422, { error: "oidc_discovery_failed" }),
    });
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.type(
      await screen.findByLabelText("Issuer URL"),
      "https://idp.example.com",
    );
    await user.type(screen.getByLabelText("Client ID"), "orkano");
    await user.type(screen.getByLabelText("Client secret"), "s3cret");
    await user.type(
      screen.getByLabelText("Allowed emails"),
      "ops@example.com",
    );
    await user.click(
      screen.getByRole("button", { name: "Connect identity provider" }),
    );

    expect(
      await screen.findByText(/did not answer OIDC discovery/),
    ).toBeInTheDocument();
  });

  it("shows the active-SSO state instead of the connect form", async () => {
    stubFetchRoutes(statusRoute(makeStatus({ oidcEnabled: true })));
    renderWithSession(<SetupWizard />);

    expect(
      await screen.findByText(/Single sign-on is active/),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Issuer URL")).not.toBeInTheDocument();
  });

  it("reopens the connect form for a credential rotation", async () => {
    stubFetchRoutes(statusRoute(makeStatus({ oidcEnabled: true })));
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.click(
      await screen.findByRole("button", { name: "Reconfigure" }),
    );
    expect(screen.getByLabelText("Issuer URL")).toBeInTheDocument();
  });

  it("prompts a restart after a rotation while SSO stays active", async () => {
    stubFetchRoutes(
      statusRoute(makeStatus({ oidcEnabled: true, oidcPendingRestart: true })),
    );
    renderWithSession(<SetupWizard />);

    expect(
      await screen.findByText(/Restart the dashboard to apply it/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/rollout restart deployment\/orkano-dashboard/),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Issuer URL")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Reconfigure" }),
    ).not.toBeInTheDocument();
  });

  it("warns when the redirect URL is host-derived, and not when pinned", async () => {
    stubFetchRoutes(statusRoute(makeStatus()));
    const { unmount } = renderWithSession(<SetupWizard />);
    expect(await screen.findByText(/written permanently/)).toBeInTheDocument();
    unmount();

    stubFetchRoutes(
      statusRoute(
        makeStatus({
          publicUrlConfigured: true,
          oidcRedirectUrl: "https://dash.example.com/api/auth/oidc/callback",
        }),
      ),
    );
    renderWithSession(<SetupWizard />);
    expect(await screen.findByLabelText("Issuer URL")).toBeInTheDocument();
    expect(screen.queryByText(/written permanently/)).not.toBeInTheDocument();
    // The SERVER-authoritative URL is displayed, never this window's origin —
    // an admin on a port-forward must register the pinned URL at the IdP.
    expect(
      screen.getByText("https://dash.example.com/api/auth/oidc/callback"),
    ).toBeInTheDocument();
  });

  it("surfaces a manifest-start refusal with the webhook-URL copy", async () => {
    // The status said configured but the manifest start 409s (e.g. the env
    // changed between polls) — the mapped copy shows instead of a raw code.
    stubFetchRoutes({
      ...statusRoute(makeStatus({ webhookUrlConfigured: true })),
      "GET /api/github/app/manifest": () =>
        jsonResponse(409, { error: "webhook_url_not_configured" }),
    });
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.click(
      await screen.findByRole("button", { name: "Create GitHub App" }),
    );
    expect(
      await screen.findByText(/public webhook URL is not configured/),
    ).toBeInTheDocument();
  });

  it("form-POSTs the manifest to GitHub", async () => {
    const submitted: { action: string; manifest: string }[] = [];
    const submitSpy = vi
      .spyOn(HTMLFormElement.prototype, "submit")
      .mockImplementation(function (this: HTMLFormElement) {
        const input = this.elements.namedItem("manifest");
        submitted.push({
          action: this.action,
          manifest: input instanceof HTMLInputElement ? input.value : "",
        });
      });
    const mock = stubFetchRoutes({
      ...statusRoute(makeStatus({ webhookUrlConfigured: true })),
      "GET /api/github/app/manifest": () =>
        jsonResponse(200, {
          postUrl: "https://github.com/organizations/acme/settings/apps/new?state=s1",
          manifest: '{"name":"orkano"}',
        }),
    });
    renderWithSession(<SetupWizard />);
    const user = userEvent.setup();

    await user.type(
      await screen.findByLabelText("Organization (optional)"),
      "acme",
    );
    await user.click(
      screen.getByRole("button", { name: "Create GitHub App" }),
    );

    await waitFor(() => {
      expect(submitSpy).toHaveBeenCalledTimes(1);
    });
    expect(submitted[0]?.action).toBe(
      "https://github.com/organizations/acme/settings/apps/new?state=s1",
    );
    expect(submitted[0]?.manifest).toBe('{"name":"orkano"}');
    // The org rode the manifest request.
    const manifestCall = mock.mock.calls.find(([url]) =>
      String(url).includes("/api/github/app/manifest"),
    );
    expect(String(manifestCall?.[0])).toContain("org=acme");
  });

  it("shows the connected GitHub state with the receiver rollout prompt", async () => {
    stubFetchRoutes(
      statusRoute(
        makeStatus({
          webhookUrlConfigured: true,
          github: {
            connected: true,
            appSlug: "orkano-acme",
            appId: "424242",
            // A fresh connect (relative to the real clock — the recency window
            // drives the prompt), so the restart reminder shows.
            connectedAt: new Date().toISOString(),
          },
        }),
      ),
    );
    renderWithSession(<SetupWizard />);

    expect(
      await screen.findByText(/Connected as orkano-acme \(App ID 424242\)/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/rollout restart deployment\/orkano-receiver/),
    ).toBeInTheDocument();

    // The Copy affordance puts the exact command on the clipboard (buttons are
    // named per command — several Copy buttons can share the screen).
    const user = userEvent.setup();
    await user.click(
      screen.getByRole("button", {
        name: "Copy kubectl -n orkano-system rollout restart deployment/orkano-receiver",
      }),
    );
    expect(await window.navigator.clipboard.readText()).toBe(
      "kubectl -n orkano-system rollout restart deployment/orkano-receiver",
    );
  });

  it("drops the rollout prompt once the connect is no longer fresh", async () => {
    stubFetchRoutes(
      statusRoute(
        makeStatus({
          webhookUrlConfigured: true,
          github: {
            connected: true,
            appSlug: "orkano-acme",
            connectedAt: "2026-01-01T00:00:00Z",
          },
        }),
      ),
    );
    renderWithSession(<SetupWizard />);

    expect(
      await screen.findByText(/Push webhooks are delivered/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/rollout restart deployment\/orkano-receiver/),
    ).not.toBeInTheDocument();
  });

  it("renders the domain check message", async () => {
    stubFetchRoutes(
      statusRoute(
        makeStatus({
          checks: freshChecks.map((c) =>
            c.id === "domains.tls-ready"
              ? {
                  ...c,
                  outcome: "fail" as const,
                  message: "2 domain(s), none with a ready certificate yet",
                }
              : c,
          ),
        }),
      ),
    );
    renderWithSession(<SetupWizard />);

    expect(
      await screen.findByText(/2 domain\(s\), none with a ready certificate/),
    ).toBeInTheDocument();
    expect(screen.getByText(/Let's Encrypt staging/)).toBeInTheDocument();
  });
});
