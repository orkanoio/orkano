import { errorFromResponse } from "@/lib/api";

// Minimal Server-Sent-Events consumption over fetch for the live-logs stream
// (server: dashboard/internal/server/logs.go). fetch instead of EventSource:
// a non-2xx pre-stream failure surfaces as a normal ApiError (EventSource
// hides the status and body), there is no implicit auto-reconnect replaying
// the tail after the server's eof, and tests drive it with the same stubbed
// fetch as every other call.

export interface AppLogHandlers {
  onLine: (pod: string, line: string) => void;
  // One pod's stream failed (the server's `error` event); the other pods keep
  // streaming.
  onStreamError: (pod: string) => void;
  // The server signalled end-of-stream (`eof` event): every pod stream ended.
  onEof: () => void;
}

// streamAppLogs consumes one log stream until the server ends it or signal
// aborts (which resolves quietly — the caller chose to stop). A non-2xx
// response rejects with ApiError before any event is delivered.
export async function streamAppLogs(
  path: string,
  signal: AbortSignal,
  handlers: AppLogHandlers,
): Promise<void> {
  let res: Response;
  try {
    res = await fetch(path, {
      signal,
      headers: { Accept: "text/event-stream" },
    });
  } catch (err) {
    if (signal.aborted) {
      return;
    }
    throw err;
  }
  if (!res.ok) {
    throw await errorFromResponse(res);
  }
  if (!res.body) {
    return;
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        return;
      }
      buffer += decoder.decode(value, { stream: true });
      let boundary = buffer.indexOf("\n\n");
      while (boundary >= 0) {
        dispatchEvent(buffer.slice(0, boundary), handlers);
        buffer = buffer.slice(boundary + 2);
        boundary = buffer.indexOf("\n\n");
      }
    }
  } catch (err) {
    if (signal.aborted) {
      return;
    }
    throw err;
  }
}

// dispatchEvent parses one SSE event block (the lines between blank-line
// separators) and routes it. The server frames every event as a single
// `data:` line of JSON, optionally preceded by an `event:` line; bare `:`
// comment lines are its keepalive heartbeats.
function dispatchEvent(block: string, handlers: AppLogHandlers): void {
  let event = "";
  let data = "";
  for (const raw of block.split("\n")) {
    const line = raw.endsWith("\r") ? raw.slice(0, -1) : raw;
    if (line.startsWith(":")) {
      continue; // heartbeat comment
    }
    if (line.startsWith("event:")) {
      event = stripFieldValue(line.slice("event:".length));
    } else if (line.startsWith("data:")) {
      data = stripFieldValue(line.slice("data:".length));
    }
  }
  if (data === "") {
    return;
  }
  let payload: unknown;
  try {
    payload = JSON.parse(data);
  } catch {
    return; // a malformed frame is dropped, never crashes the stream
  }
  if (typeof payload !== "object" || payload === null) {
    return;
  }
  const fields = payload as Record<string, unknown>;
  const pod = typeof fields.pod === "string" ? fields.pod : "";
  switch (event) {
    case "":
      if (typeof fields.line === "string") {
        handlers.onLine(pod, fields.line);
      }
      break;
    case "error":
      handlers.onStreamError(pod);
      break;
    case "eof":
      handlers.onEof();
      break;
  }
}

// Per the SSE spec, a single leading space after the field colon is not part
// of the value.
function stripFieldValue(v: string): string {
  return v.startsWith(" ") ? v.slice(1) : v;
}
