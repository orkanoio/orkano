import { describe, expect, it } from "vitest";

import { readGitHubResult, stripGitHubParams } from "@/setup/github";
import { githubErrorMessage } from "@/setup/messages";

describe("readGitHubResult", () => {
  it("reads a successful connect", () => {
    window.history.replaceState(null, "", "/?github=connected");
    expect(readGitHubResult()).toEqual({ ok: true });
  });

  it("maps a callback error code", () => {
    window.history.replaceState(null, "", "/?github_error=state_mismatch");
    const result = readGitHubResult();
    expect(result?.ok).toBe(false);
    if (result && !result.ok) {
      expect(result.message).toMatch(/did not match/);
    }
  });

  it("degrades an unknown code instead of crashing", () => {
    expect(githubErrorMessage("novel_code")).toContain("novel_code");
  });

  it("returns null without the params", () => {
    window.history.replaceState(null, "", "/");
    expect(readGitHubResult()).toBeNull();
  });
});

describe("stripGitHubParams", () => {
  it("scrubs both params and preserves the rest of the URL", () => {
    window.history.replaceState(
      null,
      "",
      "/?github=connected&keep=1#/setup",
    );
    stripGitHubParams();
    expect(window.location.search).toBe("?keep=1");
    expect(window.location.hash).toBe("#/setup");

    window.history.replaceState(null, "", "/?github_error=no_flow");
    stripGitHubParams();
    expect(window.location.search).toBe("");
  });

  it("is a no-op without the params", () => {
    window.history.replaceState(null, "", "/?other=x");
    stripGitHubParams();
    expect(window.location.search).toBe("?other=x");
  });
});
