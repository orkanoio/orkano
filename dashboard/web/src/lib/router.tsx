import { useSyncExternalStore, type ReactNode } from "react";

// A hash-based router in ~40 lines instead of a router dependency: the SPA is
// served from a single embedded index.html, so hash routes need no server
// cooperation, deep links survive reloads, and the onboarding wizard
// (sub-commit 6) can reuse it. Deliberately no nesting, params, or data
// loading — screens parse their own path segments.

function subscribe(onChange: () => void): () => void {
  window.addEventListener("hashchange", onChange);
  return () => {
    window.removeEventListener("hashchange", onChange);
  };
}

function currentPath(): string {
  const hash = window.location.hash.replace(/^#/, "");
  return hash.startsWith("/") ? hash : "/";
}

// useRoute returns the current hash path ("/", "/apps/my-app", ...) and
// re-renders on navigation.
export function useRoute(): string {
  return useSyncExternalStore(subscribe, currentPath);
}

export function navigate(path: string): void {
  window.location.hash = path;
}

// routeSegments splits a path into decoded segments ("/apps/my%2Fapp" →
// ["apps", "my/app"]).
export function routeSegments(path: string): string[] {
  return path
    .split("/")
    .filter((s) => s !== "")
    .map(decodeURIComponent);
}

export function Link({
  to,
  className,
  children,
  "aria-current": ariaCurrent,
}: {
  to: string;
  className?: string;
  children: ReactNode;
  "aria-current"?: "page";
}) {
  return (
    <a href={`#${to}`} className={className} aria-current={ariaCurrent}>
      {children}
    </a>
  );
}
