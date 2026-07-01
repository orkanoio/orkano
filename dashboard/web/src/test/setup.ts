// Shared vitest setup (vite.config.ts test.setupFiles): jest-dom matchers on
// vitest's expect, plus per-test cleanup. Globals stay off — tests import
// describe/it/expect/vi explicitly — so RTL's auto-cleanup doesn't run and the
// unmount happens here.
import "@testing-library/jest-dom/vitest";

import { cleanup } from "@testing-library/react";
import { afterEach, vi } from "vitest";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
