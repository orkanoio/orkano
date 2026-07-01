import { afterEach, describe, expect, it, vi } from "vitest";

import { findCondition, formatAge, readiness } from "@/lib/format";

describe("formatAge", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it.each([
    [30, "now"],
    [90, "1m"],
    [59 * 60, "59m"],
    [3 * 3600, "3h"],
    [49 * 3600, "2d"],
  ])("renders an age of %d seconds as %s", (seconds, want) => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-02T12:00:00Z"));
    const then = new Date(Date.now() - seconds * 1000).toISOString();
    expect(formatAge(then)).toBe(want);
  });

  it("renders missing or unparseable timestamps as a dash", () => {
    expect(formatAge(null)).toBe("—");
    expect(formatAge(undefined)).toBe("—");
    expect(formatAge("not-a-date")).toBe("—");
  });
});

describe("readiness", () => {
  it("summarizes the Ready condition", () => {
    expect(readiness(undefined)).toEqual({ label: "Pending", tone: "pending" });
    expect(
      readiness([{ type: "Ready", status: "Unknown" }]),
    ).toEqual({ label: "Pending", tone: "pending" });
    expect(
      readiness([{ type: "Ready", status: "True", message: "ok" }]),
    ).toEqual({ label: "Ready", tone: "ok", message: "ok" });
    expect(
      readiness([
        {
          type: "Ready",
          status: "False",
          reason: "ProvisionFailed",
          message: "cannot shrink",
        },
      ]),
    ).toEqual({
      label: "ProvisionFailed",
      tone: "failed",
      message: "cannot shrink",
    });
  });

  it("finds a condition by type", () => {
    const conds = [
      { type: "Ready", status: "True" } as const,
      { type: "CertificateReady", status: "False" } as const,
    ];
    expect(findCondition(conds, "CertificateReady")?.status).toBe("False");
    expect(findCondition(conds, "Missing")).toBeUndefined();
  });
});
