import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { vi } from "vitest";

// renderWithClient mounts ui under a fresh QueryClient with retries off so a
// mocked 4xx/5xx surfaces immediately instead of after the production retry.
export function renderWithClient(ui: ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
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
