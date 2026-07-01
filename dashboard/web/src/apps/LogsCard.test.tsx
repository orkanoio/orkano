import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { LogsCard } from "@/apps/LogsCard";
import { jsonResponse, renderWithSession, stubFetchRoutes } from "@/test/helpers";

function sseResponse(body: string): Response {
  return new Response(body, {
    status: 200,
    headers: { "Content-Type": "text/event-stream" },
  });
}

describe("LogsCard", () => {
  it("streams on demand and shows lines with a short pod gutter", async () => {
    const mock = stubFetchRoutes({
      "GET /api/apps/web/logs": () =>
        sseResponse(
          'data: {"pod":"web-5b9f7-abcde","line":"listening on :8080"}\n\n' +
            'event: eof\ndata: {"reason":"streams ended"}\n\n',
        ),
    });
    renderWithSession(<LogsCard appName="web" />);

    // No connection until asked.
    expect(mock).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Stream logs" }));

    expect(await screen.findByText("listening on :8080")).toBeInTheDocument();
    expect(screen.getByText("5b9f7-abcde")).toBeInTheDocument();
    expect(await screen.findByText("stream ended")).toBeInTheDocument();
  });

  it("surfaces per-pod stream failures", async () => {
    stubFetchRoutes({
      "GET /api/apps/web/logs": () =>
        sseResponse(
          'event: error\ndata: {"pod":"web-1","error":"stream_error"}\n\n' +
            'event: eof\ndata: {"reason":"streams ended"}\n\n',
        ),
    });
    renderWithSession(<LogsCard appName="web" />);

    await userEvent.click(screen.getByRole("button", { name: "Stream logs" }));

    expect(
      await screen.findByText(/Some pod streams failed: web-1/),
    ).toBeInTheDocument();
  });

  it("surfaces a pre-stream failure as readable copy", async () => {
    stubFetchRoutes({
      "GET /api/apps/web/logs": () =>
        jsonResponse(503, { error: "unavailable" }),
    });
    renderWithSession(<LogsCard appName="web" />);

    await userEvent.click(screen.getByRole("button", { name: "Stream logs" }));

    expect(
      await screen.findByText(/cluster API is unavailable/),
    ).toBeInTheDocument();
  });

  it("stops the stream on demand", async () => {
    // A never-ending stream: the reader stays open until aborted.
    stubFetchRoutes({
      "GET /api/apps/web/logs": () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(
                new TextEncoder().encode(
                  'data: {"pod":"web-1","line":"tick"}\n\n',
                ),
              );
              // Never closed — follow mode.
            },
          }),
          { status: 200 },
        ),
    });
    renderWithSession(<LogsCard appName="web" />);
    const user = userEvent.setup();

    await user.click(screen.getByRole("button", { name: "Stream logs" }));
    expect(await screen.findByText("tick")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Stop" }));
    expect(
      screen.getByRole("button", { name: "Stream logs" }),
    ).toBeInTheDocument();
  });
});
