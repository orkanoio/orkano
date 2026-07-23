import { useQuery } from "@tanstack/react-query";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { CommandLine } from "@/components/CommandLine";
import { StatusDot } from "@/components/StatusBadge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  doctorKey,
  fetchDoctorReport,
  type DoctorReport,
  type SetupCheck,
} from "@/lib/api";

import { blockerLabel, checkLabel } from "./messages";

// DoctorPage runs the read-only doctor check subset on mount and renders the
// outcome per check with the message/remediation debug detail. Deliberately no
// refetchInterval — each run is ~10 cluster reads — but a "Run checks again"
// button re-runs on demand.
export function DoctorPage() {
  const query = useQuery({ queryKey: doctorKey, queryFn: fetchDoctorReport });

  return (
    <section className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <h1 className="font-display text-3xl font-medium tracking-tight text-white">
          Doctor
        </h1>
        <p className="text-muted-foreground text-sm">
          Read-only hardening checks, run live against the cluster as the
          dashboard's viewer identity.
        </p>
      </div>

      {query.isPending && (
        <p className="font-mono text-xs text-muted-foreground">Loading…</p>
      )}
      {query.isError && <ApiErrorAlert error={query.error} />}
      {query.data && (
        <Report
          report={query.data}
          onRerun={() => {
            void query.refetch();
          }}
          rerunning={query.isFetching}
        />
      )}
    </section>
  );
}

function Report({
  report,
  onRerun,
  rerunning,
}: {
  report: DoctorReport;
  onRerun: () => void;
  rerunning: boolean;
}) {
  return (
    <>
      <StatusBanner report={report} />
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-1">
          <p className="font-display text-2xl font-medium tracking-tight text-white">
            {`Hardening score: ${report.score.value.toString()}%`}
          </p>
          <p className="text-muted-foreground text-sm">
            {`${report.score.passed.toString()} of ${report.score.scored.toString()} applicable checks passed`}
          </p>
        </div>
        <Button
          type="button"
          variant="outline"
          disabled={rerunning}
          onClick={onRerun}
        >
          {rerunning ? "Checking…" : "Run checks again"}
        </Button>
      </div>
      <div className="flex flex-col gap-3">
        {report.checks.map((check) => (
          <CheckCard key={check.id} check={check} />
        ))}
      </div>
      <FooterNote />
    </>
  );
}

function StatusBanner({ report }: { report: DoctorReport }) {
  if (report.status === "healthy") {
    // "healthy" mirrors the runner's ExitCode, which warnings never change, so
    // a warning-severity check can still fail/error/block under this status.
    // Only claim a clean baseline when nothing needs attention.
    const attention =
      report.summary.failed + report.summary.errored + report.summary.blocked;
    return (
      <Alert variant="success">
        <AlertDescription>
          {attention === 0
            ? "All checks passed — this install meets the hardening baseline."
            : attention === 1
              ? "No critical issues — 1 warning-level check needs attention below."
              : `No critical issues — ${attention.toString()} warning-level checks need attention below.`}
        </AlertDescription>
      </Alert>
    );
  }
  if (report.status === "unhealthy") {
    const n = report.summary.failed;
    return (
      <Alert variant="destructive">
        <AlertDescription>
          {`${n.toString()} ${n === 1 ? "check" : "checks"} failed — review the details below.`}
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <Alert>
      <AlertDescription>
        Some checks could not be determined — the report below is incomplete.
      </AlertDescription>
    </Alert>
  );
}

function CheckCard({ check }: { check: SetupCheck }) {
  const showMessage =
    check.outcome !== "pass" &&
    check.message !== undefined &&
    check.message !== "";
  // Remediation is actionable only for the outcomes the user can act on now
  // (format.go's rule): a blocked check never ran and a skipped one is N/A.
  const showRemediation =
    (check.outcome === "fail" || check.outcome === "error") &&
    check.remediation !== undefined &&
    check.remediation !== "";
  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle className="font-display text-base tracking-tight text-white">
            {checkLabel(check.id)}
          </CardTitle>
          <div className="flex items-center gap-2">
            <Badge variant="secondary">{check.severity}</Badge>
            <OutcomeBadge check={check} />
          </div>
        </div>
      </CardHeader>
      {(showMessage || showRemediation) && (
        <CardContent className="flex flex-col gap-3">
          {showMessage && (
            <p className="text-muted-foreground font-mono text-xs leading-relaxed">
              {check.message}
            </p>
          )}
          {showRemediation && (
            <pre className="bg-terminal text-foreground overflow-x-auto rounded-lg border p-3 font-mono text-xs whitespace-pre-wrap">
              {check.remediation}
            </pre>
          )}
        </CardContent>
      )}
    </Card>
  );
}

// OutcomeBadge renders a doctor outcome. Unlike the wizard's variant, a plain
// "fail" reads as "Failed" (destructive) — the doctor page is a status report,
// not a to-do list, so a failing check is a problem, not pending work. The
// message rides both title (hover) and aria-label, since a title on a
// non-interactive span never reaches a screen reader.
function OutcomeBadge({ check }: { check: SetupCheck }) {
  const render = (
    label: string,
    variant: "success" | "warning" | "secondary" | "destructive",
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
      return render("Passed", "success");
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
      return render("Failed", "destructive");
  }
}

// FooterNote names the two checks the dashboard cannot run (they need pod
// creation or a Secret read the viewer identity deliberately lacks) and points
// at the CLI that runs the full set.
function FooterNote() {
  return (
    <Card>
      <CardContent className="flex flex-col gap-2">
        <p className="text-muted-foreground text-sm">
          Two checks run only from the command line: the live network-policy
          probe (it creates canary pods) and the target-Secret existence leg of
          the secret-store check. Run the full set from a machine with cluster
          access:
        </p>
        <CommandLine command="orkano doctor" />
      </CardContent>
    </Card>
  );
}
