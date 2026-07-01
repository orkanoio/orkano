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
  // The hash router's state lives on window.location — reset it so a test
  // that navigated doesn't route the next test's shell.
  window.history.replaceState(null, "", window.location.pathname);
});
