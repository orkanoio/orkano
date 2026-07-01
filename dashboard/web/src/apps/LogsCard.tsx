import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { appLogsPath } from "@/lib/api";
import { apiErrorMessage } from "@/lib/errors";
import { streamAppLogs } from "@/lib/sse";

// Keep the last N lines client-side; the server already bounds the replayed
// history (tail default 200, cap 5000).
const maxLines = 2000;

interface LogLine {
  id: number;
  pod: string;
  line: string;
}

type StreamState = "idle" | "streaming" | "ended" | "failed";

// LogsCard is the live-logs viewer over the SSE endpoint (M2.6 sub-commit 1).
// The stream opens only on demand — rendering an app's page must not hold a
// pod-log connection open.
export function LogsCard({ appName }: { appName: string }) {
  const [active, setActive] = useState(false);
  const [lines, setLines] = useState<LogLine[]>([]);
  const [state, setState] = useState<StreamState>("idle");
  const [failure, setFailure] = useState("");
  const [podErrors, setPodErrors] = useState<string[]>([]);
  const nextID = useRef(0);
  const scrollRef = useRef<HTMLDivElement>(null);
  // Auto-scroll follows the tail only while the user is already at the bottom;
  // scrolling up to read pins the view.
  const stickToBottom = useRef(true);

  useEffect(() => {
    if (!active) {
      return;
    }
    const ctrl = new AbortController();
    setLines([]);
    setPodErrors([]);
    setFailure("");
    setState("streaming");
    streamAppLogs(appLogsPath(appName), ctrl.signal, {
      onLine: (pod, line) => {
        const id = nextID.current++;
        setLines((prev) => {
          const next = [...prev, { id, pod, line }];
          return next.length > maxLines
            ? next.slice(next.length - maxLines)
            : next;
        });
      },
      onStreamError: (pod) => {
        setPodErrors((prev) => (prev.includes(pod) ? prev : [...prev, pod]));
      },
      onEof: () => {
        setState("ended");
      },
    })
      // An aborted stream (Stop, unmount, StrictMode's mount-cycle) resolves
      // quietly — its settle callbacks must not touch state a NEWER stream owns.
      .then(() => {
        if (!ctrl.signal.aborted) {
          setState((s) => (s === "streaming" ? "ended" : s));
        }
      })
      .catch((err: unknown) => {
        if (!ctrl.signal.aborted) {
          setFailure(apiErrorMessage(err));
          setState("failed");
        }
      });
    return () => {
      ctrl.abort();
    };
  }, [active, appName]);

  useEffect(() => {
    const el = scrollRef.current;
    if (el && stickToBottom.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [lines]);

  return (
    <Card>
      <CardHeader>
        <CardTitle>Logs</CardTitle>
        <CardDescription>
          Live pod output, streamed under the read-only viewer identity.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <div className="flex items-center gap-3">
          <Button
            type="button"
            variant={active ? "outline" : "default"}
            size="sm"
            onClick={() => {
              setActive((a) => !a);
              if (active) {
                setState("idle");
              }
            }}
          >
            {active ? "Stop" : "Stream logs"}
          </Button>
          {state === "streaming" && (
            <span className="text-muted-foreground text-xs">streaming…</span>
          )}
          {state === "ended" && (
            <span className="text-muted-foreground text-xs">stream ended</span>
          )}
        </div>
        {state === "failed" && (
          <p className="text-destructive text-sm">{failure}</p>
        )}
        {podErrors.length > 0 && (
          <p className="text-destructive text-sm">
            Some pod streams failed: {podErrors.join(", ")}
          </p>
        )}
        {(active || lines.length > 0) && (
          <div
            ref={scrollRef}
            onScroll={(e) => {
              const el = e.currentTarget;
              stickToBottom.current =
                el.scrollTop + el.clientHeight >= el.scrollHeight - 40;
            }}
            className="bg-muted max-h-96 overflow-y-auto rounded-md p-3 font-mono text-xs"
          >
            {lines.length === 0 ? (
              <p className="text-muted-foreground">Waiting for output…</p>
            ) : (
              lines.map((l) => (
                <div key={l.id} className="whitespace-pre-wrap break-all">
                  <span className="text-muted-foreground" title={l.pod}>
                    {shortPod(l.pod, appName)}{" "}
                  </span>
                  {l.line}
                </div>
              ))
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// shortPod trims the app-name prefix a Deployment pod carries
// ("myapp-5b9f7-abcde" → "5b9f7-abcde") so the gutter stays narrow.
function shortPod(pod: string, appName: string): string {
  return pod.startsWith(`${appName}-`) ? pod.slice(appName.length + 1) : pod;
}
