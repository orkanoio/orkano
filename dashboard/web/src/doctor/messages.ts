// checkLabels maps the doctor check IDs onto the friendly headings the page
// shows — a check ID is a machine identifier (stable across --json/CI), never
// UI copy. An unmapped ID falls back to its raw form so a newly added check is
// still legible before this map catches up.
const checkLabels: Record<string, string> = {
  "platform.components-ready": "Platform components",
  "exposure.dashboard-not-public": "Dashboard exposure",
  "tls.certificate-expiry": "TLS certificates",
  "backup.etcd-snapshot-age": "etcd backups",
  "secrets.store-health": "Secret store health",
  "features.unsafe-disabled": "Unsafe features",
};

export function checkLabel(id: string): string {
  return checkLabels[id] ?? id;
}

// blockerLabel names the prerequisite a blocked check is waiting on, in human
// terms. The doctor set has no Requires today, so this is a defensive path;
// it reuses the friendly labels above, degrading to a generic phrase.
export function blockerLabel(id: string | undefined): string {
  return (id && checkLabels[id]) || "a prerequisite";
}
