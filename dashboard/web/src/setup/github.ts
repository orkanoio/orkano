import type { GitHubManifestStart } from "@/lib/api";

import { githubErrorMessage } from "./messages";

// The manifest-flow callback redirects to "/?github=connected" on success and
// "/?github_error=<code>" on failure (server/github.go). Read/strip mirror
// auth/sso.tsx's StrictMode-safe split: a pure read for the state initializer,
// a separate scrub so a reload cannot re-surface a stale result.

export type GitHubResult =
  | { ok: true }
  | { ok: false; message: string };

export function readGitHubResult(): GitHubResult | null {
  const params = new URLSearchParams(window.location.search);
  if (params.get("github") === "connected") {
    return { ok: true };
  }
  const code = params.get("github_error");
  return code ? { ok: false, message: githubErrorMessage(code) } : null;
}

export function stripGitHubParams(): void {
  const url = new URL(window.location.href);
  if (!url.searchParams.has("github") && !url.searchParams.has("github_error")) {
    return;
  }
  url.searchParams.delete("github");
  url.searchParams.delete("github_error");
  window.history.replaceState(null, "", url);
}

// postManifestToGitHub leaves the SPA: it builds a real form POST carrying the
// manifest JSON (GitHub's manifest flow accepts only a browser form submit,
// not fetch — the response is GitHub's own App-creation screen) and submits
// it. The form never enters React's tree; the navigation unloads the page.
export function postManifestToGitHub(start: GitHubManifestStart): void {
  const form = document.createElement("form");
  form.method = "post";
  form.action = start.postUrl;
  const input = document.createElement("input");
  input.type = "hidden";
  input.name = "manifest";
  input.value = start.manifest;
  form.appendChild(input);
  document.body.appendChild(form);
  try {
    form.submit();
  } finally {
    // submit() queues the navigation; removing the node immediately after is
    // a no-op in a real browser and keeps jsdom (where tests stub submit)
    // free of stray forms leaking across tests.
    form.remove();
  }
}
