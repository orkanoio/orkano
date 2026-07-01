import type { Condition } from "@/lib/api";

// formatAge renders a timestamp kubectl-style: the single largest unit, so a
// glance gives magnitude ("3d", "2h", "5m"). Sub-minute is "now"; a missing
// or unparseable timestamp renders as an em dash.
export function formatAge(timestamp: string | null | undefined): string {
  if (!timestamp) {
    return "—";
  }
  const then = Date.parse(timestamp);
  if (Number.isNaN(then)) {
    return "—";
  }
  const seconds = Math.floor((Date.now() - then) / 1000);
  if (seconds < 60) {
    return "now";
  }
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    return `${minutes.toString()}m`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    return `${hours.toString()}h`;
  }
  return `${Math.floor(hours / 24).toString()}d`;
}

export function findCondition(
  conditions: Condition[] | undefined,
  type: string,
): Condition | undefined {
  return conditions?.find((c) => c.type === type);
}

// Readiness summarizes a kind's Ready condition for a status badge.
export interface Readiness {
  label: string;
  // Maps onto the Badge variants: ok → default (teal), failed → destructive,
  // pending → secondary.
  tone: "ok" | "failed" | "pending";
  message?: string;
}

// readiness collapses the Ready condition every Orkano kind carries into a
// badge: True → the (short) reason or "Ready", False → the reason, absent or
// Unknown → "Pending" (a fresh object the operator has not observed yet).
export function readiness(conditions: Condition[] | undefined): Readiness {
  const ready = findCondition(conditions, "Ready");
  if (!ready || ready.status === "Unknown") {
    return { label: "Pending", tone: "pending" };
  }
  if (ready.status === "True") {
    return { label: "Ready", tone: "ok", message: ready.message };
  }
  return {
    label: ready.reason ?? "NotReady",
    tone: "failed",
    message: ready.message,
  };
}
