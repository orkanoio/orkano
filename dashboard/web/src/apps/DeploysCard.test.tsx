import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { DeploysCard } from "@/apps/DeploysCard";
import {
  emptyResponse,
  jsonResponse,
  renderWithSession,
  stubFetchRoutes,
} from "@/test/helpers";

function build(name: string, phase: "Succeeded" | "Failed", commit: string) {
  return {
    name,
    creationTimestamp: "2026-07-20T10:00:00Z",
    spec: {
      appName: "web",
      commit,
      source: { github: { repo: "orkanoio/web" } },
      strategy: { strategy: "Dockerfile" },
    },
    status: {
      phase,
      jobRef: { namespace: "orkano-builds", name: `${name}-job` },
      startedAt: "2026-07-20T10:00:00Z",
      completedAt: "2026-07-20T10:00:08Z",
      conditions: [
        {
          type: "Completed",
          status: phase === "Succeeded" ? "True" : "False",
          reason: phase,
          message: phase === "Failed" ? "Dockerfile command exited with status 1" : "Image pushed",
        },
      ],
    },
  };
}

function logResponse(line: string) {
  return new Response(
    `data: {"pod":"build-pod","line":"${line}"}\n\n` +
      'event: eof\ndata: {"reason":"streams ended"}\n\n',
    { status: 200, headers: { "Content-Type": "text/event-stream" } },
  );
}

describe("DeploysCard", () => {
  it("lists Build attempts and shows output for the selected history row", async () => {
    const failed = build(
      "web-failed",
      "Failed",
      "abcdef0123456789abcdef0123456789abcdef01",
    );
    const succeeded = build(
      "web-succeeded",
      "Succeeded",
      "1111111111111111111111111111111111111111",
    );
    stubFetchRoutes({
      "GET /api/apps/web/builds": () =>
        jsonResponse(200, {
          items: [failed, succeeded],
          repo: "orkanoio/web",
          automaticDeploys: true,
          githubConfigured: true,
          githubConnected: true,
        }),
      "GET /api/apps/web/builds/web-failed/logs": () =>
        logResponse("ERROR process failed"),
      "GET /api/apps/web/builds/web-succeeded/logs": () =>
        logResponse("exporting manifest"),
    });
    renderWithSession(
      <DeploysCard
        appName="web"
        source={{ github: { repo: "orkanoio/web" } }}
      />,
    );

    expect(await screen.findByText("web-failed")).toBeInTheDocument();
    expect(screen.getByText("web-succeeded")).toBeInTheDocument();
    expect(
      await screen.findByText("Dockerfile command exited with status 1"),
    ).toBeInTheDocument();
    expect(await screen.findByText("ERROR process failed")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "web-succeeded" }));
    expect(await screen.findByText("exporting manifest")).toBeInTheDocument();
    expect(screen.getByText("Image pushed")).toBeInTheDocument();
  });

  it("explains an empty history, the allowlist, and confirms Deploy now", async () => {
    let requests = 0;
    stubFetchRoutes({
      "GET /api/apps/web/builds": () =>
        jsonResponse(200, {
          items: [],
          repo: "levatax/admin-dashboard",
          automaticDeploys: false,
          githubConfigured: true,
          githubConnected: true,
        }),
      "POST /api/apps/web/deploy": () => {
        requests++;
        return emptyResponse(202);
      },
    });
    renderWithSession(
      <DeploysCard
        appName="web"
        source={{ github: { repo: "levatax/admin-dashboard" } }}
      />,
    );

    expect(
      await screen.findByText("No Build has started for this app."),
    ).toBeInTheDocument();
    expect(screen.getByText(/Automatic push deploys are off/)).toBeInTheDocument();
    expect(screen.getAllByText("levatax/admin-dashboard")).toHaveLength(2);
    expect(screen.getByRole("link", { name: "Settings" })).toHaveAttribute(
      "href",
      "#/settings",
    );

    await userEvent.click(screen.getByRole("button", { name: "Deploy now" }));
    expect(
      screen.getByRole("button", { name: "Requesting deploy…" }),
    ).toBeDisabled();
    await waitFor(() => expect(requests).toBe(1));
    expect(
      await screen.findByText(/Deploy requested\. Orkano is resolving/),
    ).toBeInTheDocument();
  });

  it("blocks on a missing GitHub App before warning about the receiver URL", async () => {
    let configured = false;
    let connected = false;
    stubFetchRoutes({
      "GET /api/apps/web/builds": () =>
        jsonResponse(200, {
          items: [],
          repo: "orkanoio/web",
          automaticDeploys: false,
          githubConfigured: configured,
          githubConnected: connected,
        }),
    });
    const view = renderWithSession(
      <DeploysCard
        appName="web"
        source={{ github: { repo: "orkanoio/web" } }}
      />,
    );

    expect(
      await screen.findByText(/not connected to GitHub/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/public webhook URL is not configured/),
    ).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Deploy now" })).toBeDisabled();
    view.unmount();

    connected = true;
    renderWithSession(
      <DeploysCard
        appName="web"
        source={{ github: { repo: "orkanoio/web" } }}
      />,
    );
    expect(
      await screen.findByText(/public webhook URL is not configured/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Deploy now" }),
    ).not.toBeDisabled();
  });

  it("keeps GitHub deploy disabled until connection state loads", async () => {
    let resolveBuilds: (response: Response) => void = () => {};
    stubFetchRoutes({
      "GET /api/apps/web/builds": () =>
        new Promise<Response>((resolve) => {
          resolveBuilds = resolve;
        }),
    });
    renderWithSession(
      <DeploysCard
        appName="web"
        source={{ github: { repo: "orkanoio/web" } }}
      />,
    );

    expect(screen.getByRole("button", { name: "Deploy now" })).toBeDisabled();
    resolveBuilds(
      jsonResponse(200, {
        items: [],
        repo: "orkanoio/web",
        automaticDeploys: true,
        githubConfigured: true,
        githubConnected: true,
      }),
    );
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: "Deploy now" }),
      ).not.toBeDisabled();
    });
  });

  it("does not show GitHub warnings for a generic Git source", async () => {
    stubFetchRoutes({
      "GET /api/apps/web/builds": () =>
        jsonResponse(200, {
          items: [],
          repo: "https://git.example.com/team/web.git",
          automaticDeploys: false,
          githubConfigured: false,
          githubConnected: false,
        }),
    });
    renderWithSession(
      <DeploysCard
        appName="web"
        source={{ git: { url: "https://git.example.com/team/web.git" } }}
      />,
    );

    expect(
      await screen.findByText("No Build has started for this app."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Automatic GitHub deploys/)).not.toBeInTheDocument();
    expect(screen.queryByText(/not connected to GitHub/)).not.toBeInTheDocument();
    expect(
      screen.queryByText(/Automatic push deploys are off/),
    ).not.toBeInTheDocument();
  });
});
