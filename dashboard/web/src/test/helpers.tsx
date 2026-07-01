import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { vi } from "vitest";

import { SessionContext, type Session } from "@/shell/session";

// renderWithClient mounts ui under a fresh QueryClient with retries off so a
// mocked 4xx/5xx surfaces immediately instead of after the production retry.
export function renderWithClient(ui: ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

// renderWithSession additionally provides the signed-in session the App and
// catalog screens read (StepUpForm's oidc branch).
export function renderWithSession(
  ui: ReactElement,
  session: Session = { username: "admin", oidc: false },
) {
  return renderWithClient(
    <SessionContext.Provider value={session}>{ui}</SessionContext.Provider>,
  );
}

// stubFetch replaces global fetch for one test (unstubbed by the shared
// afterEach) and returns the mock for call assertions.
export function stubFetch() {
  const mock = vi.fn<typeof fetch>();
  vi.stubGlobal("fetch", mock);
  return mock;
}

export function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

export function emptyResponse(status: number): Response {
  return new Response(null, { status });
}

// deferred hands out a promise plus its resolver, for pinning in-flight UI
// states (pending labels, disabled buttons) before letting the call finish.
export function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

// stubFetchRoutes installs a URL-dispatching fetch stub for screens that fire
// several queries on mount (where call-order chains would be racy). Keys are
// "METHOD /path" with any query string stripped; an unrouted call rejects
// loudly. Handlers may be called more than once (refetches).
export function stubFetchRoutes(
  routes: Record<string, (init?: RequestInit) => Response | Promise<Response>>,
) {
  const mock = stubFetch();
  mock.mockImplementation((input, init) => {
    const url =
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.href
          : input.url;
    const path = url.split("?")[0] ?? url;
    const handler = routes[`${init?.method ?? "GET"} ${path}`];
    if (!handler) {
      return Promise.reject(
        new Error(`no fetch stub for ${init?.method ?? "GET"} ${url}`),
      );
    }
    return Promise.resolve(handler(init));
  });
  return mock;
}

// requestBody decodes the JSON body of the nth fetch call.
export async function requestBody(
  mock: ReturnType<typeof stubFetch>,
  n = 0,
): Promise<unknown> {
  const call = mock.mock.calls[n];
  if (!call) {
    throw new Error(`fetch call ${n.toString()} was never made`);
  }
  const init = call[1];
  if (typeof init?.body !== "string") {
    throw new Error(`fetch call ${n.toString()} has no string body`);
  }
  return JSON.parse(init.body);
}
