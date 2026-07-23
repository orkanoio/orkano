import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { DoctorPage } from "@/doctor/DoctorPage";
import type { DoctorReport, SetupCheck } from "@/lib/api";
import { Shell } from "@/shell/Shell";
import {
  jsonResponse,
  renderWithClient,
  renderWithSession,
  stubFetchRoutes,
} from "@/test/helpers";

function makeCheck(overrides: Partial<SetupCheck> & { id: string }): SetupCheck {
  return { severity: "critical", outcome: "pass", ...overrides };
}

// The six read-only checks the dashboard face runs, in the order the server
// returns them (netpol is CLI-only, so it is absent here).
const passingChecks: SetupCheck[] = [
  makeCheck({ id: "platform.components-ready" }),
  makeCheck({ id: "exposure.dashboard-not-public" }),
  makeCheck({ id: "tls.certificate-expiry", severity: "warning" }),
  makeCheck({ id: "backup.etcd-snapshot-age", severity: "warning" }),
  makeCheck({ id: "secrets.store-health", severity: "warning" }),
  makeCheck({ id: "features.unsafe-disabled", severity: "warning" }),
];

// replaceCheck swaps one check by ID, preserving the six-entry order/count.
function replaceCheck(replacement: SetupCheck): SetupCheck[] {
  return passingChecks.map((c) =>
    c.id === replacement.id ? replacement : c,
  );
}

function makeReport(overrides?: Partial<DoctorReport>): DoctorReport {
  return {
    status: "healthy",
    score: { value: 100, earned: 26, possible: 26, passed: 6, scored: 6 },
    summary: {
      total: 6,
      passed: 6,
      failed: 0,
      errored: 0,
      blocked: 0,
      skipped: 0,
    },
    checks: passingChecks,
    checkedAt: "2026-07-23T10:00:00Z",
    ...overrides,
  };
}

const authedStatus = {
  state: "authenticated" as const,
  oidcEnabled: false,
  username: "admin",
  oidc: false,
};

describe("DoctorPage", () => {
  it("renders a healthy report with the score and pass badges", async () => {
    stubFetchRoutes({
      "GET /api/doctor": () => jsonResponse(200, makeReport()),
    });
    renderWithSession(<DoctorPage />);

    expect(await screen.findByText(/All checks passed/)).toBeInTheDocument();
    expect(screen.getByText("Hardening score: 100%")).toBeInTheDocument();
    expect(
      screen.getByText("6 of 6 applicable checks passed"),
    ).toBeInTheDocument();
    // Friendly labels, never the raw check IDs.
    expect(screen.getByText("Platform components")).toBeInTheDocument();
    expect(screen.getByText("TLS certificates")).toBeInTheDocument();
    expect(screen.queryByText("platform.components-ready")).not.toBeInTheDocument();
    expect(screen.getAllByText("Passed")).toHaveLength(6);
  });

  it("renders an unhealthy report with the failing check's message and remediation", async () => {
    const failing = makeCheck({
      id: "exposure.dashboard-not-public",
      outcome: "fail",
      message: "The dashboard Service is type LoadBalancer.",
      remediation: "Change the dashboard Service back to ClusterIP.",
    });
    stubFetchRoutes({
      "GET /api/doctor": () =>
        jsonResponse(
          200,
          makeReport({
            status: "unhealthy",
            score: { value: 83, earned: 16, possible: 26, passed: 5, scored: 6 },
            summary: {
              total: 6,
              passed: 5,
              failed: 1,
              errored: 0,
              blocked: 0,
              skipped: 0,
            },
            checks: replaceCheck(failing),
          }),
        ),
    });
    renderWithSession(<DoctorPage />);

    expect(await screen.findByText(/1 check failed/)).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(
      screen.getByText("The dashboard Service is type LoadBalancer."),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Change the dashboard Service back to ClusterIP."),
    ).toBeInTheDocument();
  });

  it("shows the indeterminate banner and an errored check's detail", async () => {
    // A CRITICAL check erroring is what drives ExitIndeterminate(2) → status
    // "indeterminate"; a warning erroring would leave the run "healthy".
    const erroring = makeCheck({
      id: "platform.components-ready",
      outcome: "error",
      message: "getting Deployment orkano-operator failed: forbidden",
      remediation: "Grant the viewer deployments get in orkano-system.",
    });
    stubFetchRoutes({
      "GET /api/doctor": () =>
        jsonResponse(
          200,
          makeReport({
            status: "indeterminate",
            score: { value: 88, earned: 16, possible: 26, passed: 5, scored: 6 },
            summary: {
              total: 6,
              passed: 5,
              failed: 0,
              errored: 1,
              blocked: 0,
              skipped: 0,
            },
            checks: replaceCheck(erroring),
          }),
        ),
    });
    renderWithSession(<DoctorPage />);

    expect(
      await screen.findByText(/could not be determined/),
    ).toBeInTheDocument();
    expect(screen.getByText("Could not check")).toBeInTheDocument();
    expect(
      screen.getByText("getting Deployment orkano-operator failed: forbidden"),
    ).toBeInTheDocument();
    // An errored check IS actionable, so its remediation shows.
    expect(
      screen.getByText("Grant the viewer deployments get in orkano-system."),
    ).toBeInTheDocument();
  });

  it("shows the waiting label for a blocked check and hides its remediation", async () => {
    // OutcomeBlocked is unreachable for the current doctor set — no read-only
    // check declares Requires — so this is a defensive-rendering pin only. To
    // stay ExitCode-consistent we pair a CRITICAL blocked check with status
    // "indeterminate" (a warning block never changes the run's ExitCode).
    const blocked = makeCheck({
      id: "exposure.dashboard-not-public",
      outcome: "blocked",
      blockers: ["platform.components-ready"],
      message: "waiting on platform.components-ready",
      remediation: "this must never render for a blocked check",
    });
    stubFetchRoutes({
      "GET /api/doctor": () =>
        jsonResponse(
          200,
          makeReport({
            status: "indeterminate",
            score: { value: 88, earned: 16, possible: 26, passed: 5, scored: 6 },
            summary: {
              total: 6,
              passed: 5,
              failed: 0,
              errored: 0,
              blocked: 1,
              skipped: 0,
            },
            checks: replaceCheck(blocked),
          }),
        ),
    });
    renderWithSession(<DoctorPage />);

    expect(
      await screen.findByText("Waiting on Platform components"),
    ).toBeInTheDocument();
    // Blocked checks never ran — no remediation, per format.go's rule.
    expect(
      screen.queryByText("this must never render for a blocked check"),
    ).not.toBeInTheDocument();
  });

  it("keeps status healthy but flags a warning-level failure", async () => {
    // A warning-severity check failing does NOT move the run off "healthy"
    // (ExitCode gates on critical only) — so the success banner must stop
    // claiming everything passed and point at the check needing attention.
    const failing = makeCheck({
      id: "tls.certificate-expiry",
      severity: "warning",
      outcome: "fail",
      message: "certificate example-tls expires in 3 days.",
      remediation: "Check cert-manager renewal.",
    });
    stubFetchRoutes({
      "GET /api/doctor": () =>
        jsonResponse(
          200,
          makeReport({
            status: "healthy",
            score: { value: 88, earned: 23, possible: 26, passed: 5, scored: 6 },
            summary: {
              total: 6,
              passed: 5,
              failed: 1,
              errored: 0,
              blocked: 0,
              skipped: 0,
            },
            checks: replaceCheck(failing),
          }),
        ),
    });
    renderWithSession(<DoctorPage />);

    expect(
      await screen.findByText(
        "No critical issues — 1 warning-level check needs attention below.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/All checks passed/)).not.toBeInTheDocument();
  });

  it("renders a skipped check without its remediation", async () => {
    // Skip is excluded from the score, so a skipped warning leaves the run
    // healthy. The skip message shows (non-pass outcome) but a skipped check
    // is N/A, so its remediation must not render.
    const skipped = makeCheck({
      id: "secrets.store-health",
      severity: "warning",
      outcome: "skip",
      message: "External Secrets is not installed; skipping.",
      remediation: "this remediation must not render for a skipped check",
    });
    stubFetchRoutes({
      "GET /api/doctor": () =>
        jsonResponse(
          200,
          makeReport({
            score: { value: 100, earned: 23, possible: 23, passed: 5, scored: 5 },
            summary: {
              total: 6,
              passed: 5,
              failed: 0,
              errored: 0,
              blocked: 0,
              skipped: 1,
            },
            checks: replaceCheck(skipped),
          }),
        ),
    });
    renderWithSession(<DoctorPage />);

    expect(await screen.findByText("Not applicable")).toBeInTheDocument();
    expect(screen.queryByText("Failed")).not.toBeInTheDocument();
    expect(
      screen.getByText("External Secrets is not installed; skipping."),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("this remediation must not render for a skipped check"),
    ).not.toBeInTheDocument();
  });

  it("surfaces an API error", async () => {
    stubFetchRoutes({
      "GET /api/doctor": () =>
        jsonResponse(500, { error: "internal_error" }),
    });
    renderWithSession(<DoctorPage />);

    expect(await screen.findByText(/internal error/)).toBeInTheDocument();
  });

  it("re-runs the checks on demand", async () => {
    let calls = 0;
    stubFetchRoutes({
      "GET /api/doctor": () => {
        calls += 1;
        return jsonResponse(200, makeReport());
      },
    });
    renderWithSession(<DoctorPage />);
    const user = userEvent.setup();

    await user.click(
      await screen.findByRole("button", { name: "Run checks again" }),
    );
    await waitFor(() => {
      expect(calls).toBe(2);
    });
  });

  it("keeps the report and retry button when a refetch fails", async () => {
    let calls = 0;
    stubFetchRoutes({
      "GET /api/doctor": () => {
        calls += 1;
        return calls === 1
          ? jsonResponse(200, makeReport())
          : jsonResponse(500, { error: "internal_error" });
      },
    });
    renderWithSession(<DoctorPage />);
    const user = userEvent.setup();

    await user.click(
      await screen.findByRole("button", { name: "Run checks again" }),
    );

    // The failed refetch surfaces the error alongside the still-mounted report
    // and its retry button — the report is gated on cached data, not isSuccess.
    expect(await screen.findByText(/internal error/)).toBeInTheDocument();
    expect(screen.getByText("Hardening score: 100%")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Run checks again" }),
    ).toBeInTheDocument();
  });

  it("notes the CLI-only checks with the command", async () => {
    stubFetchRoutes({
      "GET /api/doctor": () => jsonResponse(200, makeReport()),
    });
    renderWithSession(<DoctorPage />);

    expect(
      await screen.findByText(/run only from the command line/),
    ).toBeInTheDocument();
    expect(screen.getByText("orkano doctor")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Copy orkano doctor" }),
    ).toBeInTheDocument();
  });
});

describe("Doctor in the shell", () => {
  it("navigates to the doctor page from the nav", async () => {
    stubFetchRoutes({
      "GET /api/apps": () => jsonResponse(200, { items: [] }),
      "GET /api/doctor": () => jsonResponse(200, makeReport()),
    });
    renderWithClient(<Shell status={authedStatus} />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("link", { name: "Doctor" }));

    expect(
      await screen.findByRole("heading", { name: "Doctor" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Hardening score: 100%")).toBeInTheDocument();
  });
});
