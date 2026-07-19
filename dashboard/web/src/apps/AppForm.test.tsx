import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { AppForm } from "@/apps/AppForm";
import { makeApp } from "@/test/fixtures";
import {
  jsonResponse,
  renderWithSession,
  stubFetchRoutes,
} from "@/test/helpers";

const allFeatures = {
  features: [
    {
      id: "source.git",
      label: "Generic Git",
      description: "Bypasses the GitHub App trust boundary.",
      unsafe: true,
      enabled: true,
    },
    {
      id: "source.zip",
      label: "ZIP upload",
      description: "Bypasses Git provenance.",
      unsafe: true,
      enabled: true,
    },
    {
      id: "build.nixpacks",
      label: "Nixpacks",
      description: "Runs automatic build-plan detection.",
      unsafe: true,
      enabled: true,
    },
  ],
};

function mutationBody(
  mock: ReturnType<typeof stubFetchRoutes>,
  method: "POST" | "PUT" = "POST",
): unknown {
  const call = mock.mock.calls.find(
    (candidate) =>
      (candidate[1] as RequestInit | undefined)?.method === method,
  );
  if (typeof call?.[1]?.body !== "string") {
    throw new Error(`${method} request was never made`);
  }
  return JSON.parse(call[1].body);
}

describe("AppForm create", () => {
  it("creates a Dockerfile web app and navigates to it", async () => {
    const mock = stubFetchRoutes({
      "GET /api/features": () => jsonResponse(200, allFeatures),
      "POST /api/apps": () => jsonResponse(201, makeApp({ name: "web" })),
    });
    renderWithSession(<AppForm />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Name"), "web");
    await user.type(
      screen.getByLabelText("GitHub repository"),
      "orkanoio/example",
    );
    await user.type(screen.getByLabelText("Port"), "3000");
    await user.type(screen.getByLabelText("Health check path"), "/healthz");
    await user.click(screen.getByRole("button", { name: "Create app" }));

    expect(mutationBody(mock)).toEqual({
      name: "web",
      spec: {
        source: { github: { repo: "orkanoio/example" } },
        build: { strategy: "Dockerfile" },
        type: "Web",
        port: 3000,
        replicas: 1,
        healthCheck: { path: "/healthz" },
      },
    });
    expect(window.location.hash).toBe("#/apps/web");
  });

  it("creates a Static app with only the static build member", async () => {
    const mock = stubFetchRoutes({
      "GET /api/features": () => jsonResponse(200, allFeatures),
      "POST /api/apps": () => jsonResponse(201, makeApp({ name: "site" })),
    });
    renderWithSession(<AppForm />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Name"), "site");
    await user.type(screen.getByLabelText("GitHub repository"), "o/site");
    await user.selectOptions(screen.getByLabelText("Build"), "Static");
    await user.type(screen.getByLabelText("Directory to serve"), "public");
    await user.click(screen.getByRole("button", { name: "Create app" }));

    const body = mutationBody(mock) as {
      spec: { build: Record<string, unknown> };
    };
    expect(body.spec.build).toEqual({
      strategy: "Static",
      static: { dir: "public" },
    });
    expect(body.spec.build.dockerfile).toBeUndefined();
  });

  it("creates an enabled public Git app with Nixpacks", async () => {
    const mock = stubFetchRoutes({
      "GET /api/features": () => jsonResponse(200, allFeatures),
      "POST /api/apps": () => jsonResponse(201, makeApp({ name: "auto" })),
    });
    renderWithSession(<AppForm />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Name"), "auto");
    const gitOption = await screen.findByRole("option", {
      name: "Generic Git — unsafe",
    });
    await waitFor(() => {
      expect(gitOption).not.toBeDisabled();
    });
    await user.selectOptions(screen.getByLabelText("Source provider"), "git");
    await user.type(
      screen.getByLabelText("Public Git URL"),
      "https://git.example.com/team/auto.git",
    );
    await user.selectOptions(screen.getByLabelText("Build"), "Nixpacks");
    await user.click(screen.getByRole("button", { name: "Create app" }));

    const body = mutationBody(mock) as {
      spec: Record<string, unknown>;
    };
    expect(body.spec.source).toEqual({
      git: { url: "https://git.example.com/team/auto.git" },
    });
    expect(body.spec.build).toEqual({ strategy: "Nixpacks", nixpacks: {} });
  });

  it("omits port and healthCheck for a Worker", async () => {
    const mock = stubFetchRoutes({
      "GET /api/features": () => jsonResponse(200, allFeatures),
      "POST /api/apps": () => jsonResponse(201, makeApp({ name: "job" })),
    });
    renderWithSession(<AppForm />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Name"), "job");
    await user.type(screen.getByLabelText("GitHub repository"), "o/job");
    // Port and health check fields disappear for a Worker; anything typed
    // before the switch must not survive into the spec.
    await user.type(screen.getByLabelText("Port"), "3000");
    await user.selectOptions(screen.getByLabelText("Type"), "Worker");
    expect(screen.queryByLabelText("Port")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create app" }));

    const body = mutationBody(mock) as { spec: Record<string, unknown> };
    expect(body.spec.type).toBe("Worker");
    expect(body.spec.port).toBeUndefined();
    expect(body.spec.healthCheck).toBeUndefined();
  });

  it("validates fields client-side before any request", async () => {
    const mock = stubFetchRoutes({});
    renderWithSession(<AppForm />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Name"), "Bad_Name");
    await user.type(screen.getByLabelText("GitHub repository"), "not-a-repo");
    await user.click(screen.getByRole("button", { name: "Create app" }));

    expect(
      await screen.findByText(/Use lowercase letters, digits, and hyphens/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/owner\/repository form/),
    ).toBeInTheDocument();
    expect(
      mock.mock.calls.some(
        (call) => (call[1] as RequestInit | undefined)?.method === "POST",
      ),
    ).toBe(false);
  });

  it("maps an already_exists conflict onto readable copy", async () => {
    stubFetchRoutes({
      "GET /api/features": () => jsonResponse(200, allFeatures),
      "POST /api/apps": () => jsonResponse(409, { error: "already_exists" }),
    });
    renderWithSession(<AppForm />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Name"), "web");
    await user.type(screen.getByLabelText("GitHub repository"), "o/r");
    await user.click(screen.getByRole("button", { name: "Create app" }));

    expect(
      await screen.findByText("That name is already taken."),
    ).toBeInTheDocument();
  });
});

describe("AppForm edit", () => {
  it("round-trips fields the form does not model", async () => {
    const base = makeApp({
      name: "web",
      spec: {
        source: { github: { repo: "o/r", ref: "main" }, subPath: "svc" },
        build: { strategy: "Dockerfile", dockerfile: { path: "deploy/Dockerfile" } },
        type: "Web",
        replicas: 2,
        command: ["./server"],
        env: [{ name: "MODE", value: "prod" }],
        resources: { memory: "256Mi" },
      },
    });
    const mock = stubFetchRoutes({
      "GET /api/apps/web": () => jsonResponse(200, base),
      "PUT /api/apps/web": (init) =>
        jsonResponse(200, {
          ...base,
          spec: JSON.parse(init?.body as string).spec as object,
        }),
    });
    renderWithSession(<AppForm edit="web" />);
    const user = userEvent.setup();

    const replicas = await screen.findByLabelText("Replicas");
    expect(screen.queryByLabelText("GitHub repository")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Build")).not.toBeInTheDocument();
    await user.clear(replicas);
    await user.type(replicas, "3");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    const puts = mock.mock.calls.filter(
      (c) => (c[1] as RequestInit | undefined)?.method === "PUT",
    );
    expect(puts).toHaveLength(1);
    const body = JSON.parse((puts[0]?.[1] as RequestInit).body as string) as {
      spec: Record<string, unknown>;
    };
    expect(body.spec.replicas).toBe(3);
    // Unmodeled fields survive the whole-spec PUT.
    expect(body.spec.command).toEqual(["./server"]);
    expect(body.spec.env).toEqual([{ name: "MODE", value: "prod" }]);
    expect(body.spec.resources).toEqual({ memory: "256Mi" });
    expect(body.spec.source).toEqual({
      github: { repo: "o/r", ref: "main" },
      subPath: "svc",
    });
  });
});
