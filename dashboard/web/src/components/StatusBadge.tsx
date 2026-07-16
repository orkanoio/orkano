import { Badge } from "@/components/ui/badge";
import type { Condition } from "@/lib/api";
import { findCondition, readiness } from "@/lib/format";

// StatusDot is the landing's 6px status dot, riding inside the pill badges.
// Decorative — the pill's text carries the meaning. Exported so every status
// pill in the app (incl. the setup wizard's OutcomeBadge) shares it.
export function StatusDot() {
  return (
    <span aria-hidden="true" className="size-1.5 rounded-full bg-current" />
  );
}

// StatusBadge renders a kind's Ready condition (the message, when present,
// rides the title attribute so a failure reason is hoverable in a table row).
// Tones follow the landing: green = healthy, amber = in progress, red = failed.
export function StatusBadge({
  conditions,
}: {
  conditions: Condition[] | undefined;
}) {
  const ready = readiness(conditions);
  const variant =
    ready.tone === "ok"
      ? "success"
      : ready.tone === "failed"
        ? "destructive"
        : "warning";
  return (
    <Badge variant={variant} title={ready.message}>
      <StatusDot />
      {ready.label}
    </Badge>
  );
}

// ConditionBadge renders one named condition (the Domain's CertificateReady).
export function ConditionBadge({
  conditions,
  type,
  label,
}: {
  conditions: Condition[] | undefined;
  type: string;
  label: string;
}) {
  const cond = findCondition(conditions, type);
  if (!cond || cond.status === "Unknown") {
    return (
      <Badge variant="warning" title={cond?.message}>
        <StatusDot />
        {label}: pending
      </Badge>
    );
  }
  if (cond.status === "True") {
    return (
      <Badge variant="success" title={cond.message}>
        <StatusDot />
        {label}: ready
      </Badge>
    );
  }
  return (
    <Badge variant="destructive" title={cond.message}>
      <StatusDot />
      {label}: {cond.reason ?? "not ready"}
    </Badge>
  );
}
