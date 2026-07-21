import { Pause, Play, Radio } from "lucide-react";
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
import { streamLogs } from "@/lib/sse";
import { cn } from "@/lib/utils";

const maxLines = 2000;

interface LogLine {
  id: number;
  pod: string;
  line: string;
}

type StreamState = "idle" | "streaming" | "ended" | "failed";

export function LogsCard({ appName }: { appName: string }) {
  return <ResourceStreamCard resourceName={appName} path={appLogsPath(appName)} />;
}

export function ResourceStreamCard({
  resourceName,
  path,
  title = "Live stream",
  sticky = true,
  endedLabel = "Stream ended",
  reconnectOnEnd = false,
}: {
  resourceName: string;
  path: string;
  title?: string;
  sticky?: boolean;
  endedLabel?: string;
  reconnectOnEnd?: boolean;
}) {
  const [active, setActive] = useState(true);
  const [lines, setLines] = useState<LogLine[]>([]);
  const [state, setState] = useState<StreamState>("idle");
  const [failure, setFailure] = useState("");
  const [podErrors, setPodErrors] = useState<string[]>([]);
  const [attempt, setAttempt] = useState(0);
  const nextID = useRef(0);
  const scrollRef = useRef<HTMLDivElement>(null);
  const stickToBottom = useRef(true);
  const lastPath = useRef("");
  const replayable = active && (state === "ended" || state === "failed");

  useEffect(() => {
    if (!active) {
      return;
    }
    const ctrl = new AbortController();
    let reconnectTimer: number | undefined;
    if (lastPath.current !== path) {
      lastPath.current = path;
      setLines([]);
    }
    setPodErrors([]);
    setFailure("");
    setState("streaming");
    streamLogs(path, ctrl.signal, {
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
      .then(() => {
        if (!ctrl.signal.aborted) {
          setState((current) =>
            current === "streaming" ? "ended" : current,
          );
          if (reconnectOnEnd) {
            reconnectTimer = window.setTimeout(
              () => setAttempt((current) => current + 1),
              2_000,
            );
          }
        }
      })
      .catch((error: unknown) => {
        if (!ctrl.signal.aborted) {
          setFailure(apiErrorMessage(error));
          setState("failed");
          if (reconnectOnEnd) {
            reconnectTimer = window.setTimeout(
              () => setAttempt((current) => current + 1),
              2_000,
            );
          }
        }
      });
    return () => {
      ctrl.abort();
      if (reconnectTimer !== undefined) {
        window.clearTimeout(reconnectTimer);
      }
    };
  }, [active, attempt, path, reconnectOnEnd]);

  useEffect(() => {
    const element = scrollRef.current;
    if (element && stickToBottom.current) {
      element.scrollTop = element.scrollHeight;
    }
  }, [lines]);

  return (
    <Card
      className={cn(
        "gap-0 overflow-hidden py-0 shadow-2xl shadow-black/30",
        sticky && "sticky top-3 z-20",
      )}
    >
      <CardHeader className="grid-cols-[1fr_auto] items-center gap-3 border-b px-4 py-3">
        <div className="flex min-w-0 items-center gap-3">
          <Radio className="size-4 text-primary" aria-hidden="true" />
          <div className="min-w-0">
            <CardTitle className="font-mono text-xs">{title}</CardTitle>
            <CardDescription
              role="status"
              aria-live="polite"
              className="font-mono text-[11px]"
            >
              {streamLabel(state, active, endedLabel, reconnectOnEnd)}
            </CardDescription>
          </div>
        </div>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => {
            if (replayable) {
              setAttempt((current) => current + 1);
              return;
            }
            setActive((current) => !current);
            if (active) {
              setState("idle");
            }
          }}
        >
          {active && !replayable ? (
            <Pause data-icon="inline-start" aria-hidden="true" />
          ) : (
            <Play data-icon="inline-start" aria-hidden="true" />
          )}
          {replayable
            ? "Replay output"
            : active
              ? "Pause stream"
              : "Resume stream"}
        </Button>
      </CardHeader>
      <CardContent className="bg-terminal px-0">
        <div
          ref={scrollRef}
          onScroll={(event) => {
            const element = event.currentTarget;
            stickToBottom.current =
              element.scrollTop + element.clientHeight >=
              element.scrollHeight - 40;
          }}
          className="h-52 overflow-y-auto px-4 py-3 font-mono text-[11px] leading-relaxed sm:h-60"
        >
          {state === "failed" ? (
            <p className="text-destructive">{failure}</p>
          ) : lines.length === 0 ? (
            <p className="text-muted-foreground">
              {active ? "Waiting for output…" : "Stream paused."}
            </p>
          ) : (
            lines.map((entry) => (
              <div key={entry.id} className="whitespace-pre-wrap break-all">
                <span className="text-primary/75" title={entry.pod}>
                  {shortPod(entry.pod, resourceName)}{" "}
                </span>
                {entry.line}
              </div>
            ))
          )}
          {podErrors.length > 0 ? (
            <p className="mt-2 text-destructive">
              Some pod streams failed: {podErrors.join(", ")}
            </p>
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}

function streamLabel(
  state: StreamState,
  active: boolean,
  endedLabel: string,
  reconnectOnEnd: boolean,
): string {
  if (!active) {
    return "Paused";
  }
  switch (state) {
    case "streaming":
      return "Streaming now";
    case "ended":
      return reconnectOnEnd ? "Reconnecting…" : endedLabel;
    case "failed":
      return reconnectOnEnd ? "Reconnecting…" : "Unable to stream";
    case "idle":
      return "Connecting";
  }
}

function shortPod(pod: string, resourceName: string): string {
  return pod.startsWith(`${resourceName}-`)
    ? pod.slice(resourceName.length + 1)
    : pod;
}
