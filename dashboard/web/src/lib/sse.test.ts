import { describe, expect, it, vi } from "vitest";

import { ApiError } from "@/lib/api";
import { streamLogs } from "@/lib/sse";
import { jsonResponse, stubFetch } from "@/test/helpers";

function sseResponse(body: string): Response {
  return new Response(body, {
    status: 200,
    headers: { "Content-Type": "text/event-stream" },
  });
}

function handlers() {
  return {
    onLine: vi.fn<(pod: string, line: string) => void>(),
    onStreamError: vi.fn<(pod: string) => void>(),
    onEof: vi.fn<() => void>(),
  };
}

describe("streamLogs", () => {
  it("dispatches lines, per-pod errors, and eof; ignores heartbeats", async () => {
    stubFetch().mockResolvedValueOnce(
      sseResponse(
        'data: {"pod":"web-1","line":"hello"}\n\n' +
          ":\n\n" + // keepalive comment
          'data: {"pod":"web-2","line":"world"}\n\n' +
          'event: error\ndata: {"pod":"web-2","error":"stream_error"}\n\n' +
          'event: eof\ndata: {"reason":"streams ended"}\n\n',
      ),
    );
    const h = handlers();

    await streamLogs("/api/apps/web/logs", new AbortController().signal, h);

    expect(h.onLine.mock.calls).toEqual([
      ["web-1", "hello"],
      ["web-2", "world"],
    ]);
    expect(h.onStreamError.mock.calls).toEqual([["web-2"]]);
    expect(h.onEof).toHaveBeenCalledTimes(1);
  });

  it("reassembles events split across chunk boundaries", async () => {
    // A ReadableStream that splits one event across two reads.
    const encoder = new TextEncoder();
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode('data: {"pod":"w'));
        controller.enqueue(encoder.encode('eb-1","line":"split"}\n\n'));
        controller.close();
      },
    });
    stubFetch().mockResolvedValueOnce(
      new Response(stream, { status: 200 }),
    );
    const h = handlers();

    await streamLogs("/api/apps/web/logs", new AbortController().signal, h);

    expect(h.onLine.mock.calls).toEqual([["web-1", "split"]]);
  });

  it("rejects with the server's error code on a non-2xx response", async () => {
    stubFetch().mockResolvedValueOnce(
      jsonResponse(404, { error: "not_found" }),
    );

    const err = await streamLogs(
      "/api/apps/gone/logs",
      new AbortController().signal,
      handlers(),
    ).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 404, code: "not_found" });
  });

  it("resolves quietly when aborted", async () => {
    const ctrl = new AbortController();
    stubFetch().mockImplementationOnce(() => {
      ctrl.abort();
      return Promise.reject(
        new DOMException("The operation was aborted.", "AbortError"),
      );
    });

    await expect(
      streamLogs("/api/apps/web/logs", ctrl.signal, handlers()),
    ).resolves.toBeUndefined();
  });

  it("drops malformed frames without crashing the stream", async () => {
    stubFetch().mockResolvedValueOnce(
      sseResponse(
        "data: not-json\n\n" + 'data: {"pod":"web-1","line":"after"}\n\n',
      ),
    );
    const h = handlers();

    await streamLogs("/api/apps/web/logs", new AbortController().signal, h);

    expect(h.onLine.mock.calls).toEqual([["web-1", "after"]]);
  });
});
