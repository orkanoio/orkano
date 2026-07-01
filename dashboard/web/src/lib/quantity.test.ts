import { describe, expect, it } from "vitest";

import { parseQuantityBytes } from "@/lib/quantity";

describe("parseQuantityBytes", () => {
  it.each([
    ["10Gi", 10 * 2 ** 30],
    ["512Mi", 512 * 2 ** 20],
    ["1.5Gi", 1.5 * 2 ** 30],
    ["5G", 5e9],
    ["1073741824", 1073741824],
    [" 10Gi ", 10 * 2 ** 30],
  ])("parses %s", (q, want) => {
    expect(parseQuantityBytes(q)).toBe(want);
  });

  it.each(["", "Gi", "10gi", "-1Gi", "10 Gi", "ten"])(
    "rejects %s",
    (q) => {
      expect(parseQuantityBytes(q)).toBeNull();
    },
  );
});
