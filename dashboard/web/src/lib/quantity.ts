// parseQuantityBytes reads the storage-relevant subset of a Kubernetes
// resource.Quantity ("10Gi", "500M", "1073741824") into bytes for client-side
// grow-only and floor checks. null means "not a quantity this form accepts" —
// the server (and reconciler) stay the authority on the full grammar.
const suffixes: Record<string, number> = {
  Ki: 2 ** 10,
  Mi: 2 ** 20,
  Gi: 2 ** 30,
  Ti: 2 ** 40,
  Pi: 2 ** 50,
  Ei: 2 ** 60,
  K: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  P: 1e15,
  E: 1e18,
};

const quantityRe = /^([0-9]+(?:\.[0-9]+)?)(Ki|Mi|Gi|Ti|Pi|Ei|K|M|G|T|P|E)?$/;

export function parseQuantityBytes(quantity: string): number | null {
  const m = quantityRe.exec(quantity.trim());
  if (!m?.[1]) {
    return null;
  }
  const scale = m[2] ? suffixes[m[2]] : 1;
  if (scale === undefined) {
    return null;
  }
  return Number(m[1]) * scale;
}
