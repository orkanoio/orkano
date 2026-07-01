import { Badge } from "@/components/ui/badge";
import type { Condition } from "@/lib/api";
import { findCondition, readiness } from "@/lib/format";

// StatusBadge renders a kind's Ready condition (the message, when present,
// rides the title attribute so a failure reason is hoverable in a table row).
export function StatusBadge({
  conditions,
}: {
  conditions: Condition[] | undefined;
}) {
  const ready = readiness(conditions);
  const variant =
    ready.tone === "ok"
      ? "default"
      : ready.tone === "failed"
        ? "destructive"
        : "secondary";
  return (
    <Badge variant={variant} title={ready.message}>
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
      <Badge variant="secondary" title={cond?.message}>
        {label}: pending
      </Badge>
    );
  }
  if (cond.status === "True") {
    return (
      <Badge variant="default" title={cond.message}>
        {label}: ready
      </Badge>
    );
  }
  return (
    <Badge variant="destructive" title={cond.message}>
      {label}: {cond.reason ?? "not ready"}
    </Badge>
  );
}
