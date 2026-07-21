import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { SourceCard } from "@/apps/SourceCard";
import { makeApp } from "@/test/fixtures";
import {
  jsonResponse,
  renderWithSession,
  stubFetchRoutes,
} from "@/test/helpers";

function features(enabled: Partial<Record<string, boolean>> = {}) {
  return {
    features: [
      {
        id: "source.git",
        label: "Generic Git",
        description:
          "Public Git bypasses GitHub App installation checks and deploys manually.",
        unsafe: true,
        enabled: enabled["source.git"] ?? false,
      },
      {
        id: "source.zip",
        label: "ZIP upload",
        description: "ZIP archives bypass Git commit provenance.",
        unsafe: true,
        enabled: enabled["source.zip"] ?? false,
      },
      {
        id: "build.nixpacks",
        label: "Nixpacks",
        description: "Nixpacks automatically detects and generates a build plan.",
        unsafe: true,
        enabled: enabled["build.nixpacks"] ?? false,
      },
    ],
  };
}

function putBody(mock: ReturnType<typeof stubFetchRoutes>) {
  const call = mock.mock.calls.find(
    (candidate) =>
      (candidate[1] as RequestInit | undefined)?.method === "PUT",
  );
  if (typeof call?.[1]?.body !== "string") {
    throw new Error("PUT request was never made");
  }
  return JSON.parse(call[1].body) as {
    source: Record<string, unknown>;
    build: Record<string, unknown>;
  };
}

describe("SourceCard", () => {
  it("marks unsafe source and build choices disabled by installation", async () => {
    stubFetchRoutes({
      "GET /api/features": () => jsonResponse(200, features()),
    });
    renderWithSession(<SourceCard app={makeApp()} />);

    expect(
      await screen.findByRole("option", { name: /Generic Git.*disabled/ }),
    ).toBeDisabled();
    expect(
      screen.getByRole("option", { name: /Upload ZIP.*disabled/ }),
    ).toBeDisabled();
    expect(
      screen.getByRole("option", { name: /Nixpacks.*disabled/ }),
    ).toBeDisabled();
    expect(screen.getByText(/GitHub App is the secure default/)).toBeInTheDocument();
  });

  it("saves public Git and Nixpacks through the source-scoped endpoint", async () => {
    const base = makeApp({
      name: "api",
      spec: {
        replicas: 3,
        command: ["./serve"],
        env: [{ name: "MODE", value: "prod" }],
        resources: { memory: "512Mi" },
      },
    });
    const mock = stubFetchRoutes({
      "GET /api/features": () =>
        jsonResponse(
          200,
          features({ "source.git": true, "build.nixpacks": true }),
        ),
      "PUT /api/apps/api/source": (init) =>
        jsonResponse(200, {
          ...base,
          spec: {
            ...base.spec,
            ...(JSON.parse(init?.body as string) as object),
          },
        }),
    });
    renderWithSession(<SourceCard app={base} />);
    const user = userEvent.setup();

    const gitOption = await screen.findByRole("option", {
      name: "Generic Git — unsafe",
    });
    await waitFor(() => {
      expect(gitOption).not.toBeDisabled();
    });
    await user.selectOptions(screen.getByLabelText("Source provider"), "git");
    await user.type(
      screen.getByLabelText("Public Git URL"),
      "https://git.example.com/team/api.git",
    );
    await user.selectOptions(screen.getByLabelText("Build method"), "Nixpacks");
    await user.type(
      screen.getByLabelText("Nixpacks config path"),
      "deploy/nixpacks.toml",
    );
    expect(screen.getAllByText("Unsafe feature")).toHaveLength(2);
    await user.click(screen.getByRole("button", { name: "Save source" }));

    await screen.findByText("Source saved");
    const body = putBody(mock);
    expect(body.source).toEqual({
      git: { url: "https://git.example.com/team/api.git" },
    });
    expect(body.build).toEqual({
      strategy: "Nixpacks",
      nixpacks: { configPath: "deploy/nixpacks.toml" },
    });
    expect(body).not.toHaveProperty("spec");
  });

  it("uploads a ZIP before saving its immutable digest as the source", async () => {
    const base = makeApp({ name: "site" });
    const mock = stubFetchRoutes({
      "GET /api/features": () =>
        jsonResponse(200, features({ "source.zip": true })),
      "POST /api/apps/site/source/archive": () =>
        jsonResponse(201, {
          digest: `sha256:${"a".repeat(64)}`,
          fileName: "site.zip",
        }),
      "PUT /api/apps/site/source": (init) =>
        jsonResponse(200, {
          ...base,
          spec: {
            ...base.spec,
            ...(JSON.parse(init?.body as string) as object),
          },
        }),
    });
    renderWithSession(<SourceCard app={base} />);
    const user = userEvent.setup();

    const zipOption = await screen.findByRole("option", {
      name: "Upload ZIP — unsafe",
    });
    await waitFor(() => {
      expect(zipOption).not.toBeDisabled();
    });
    await user.selectOptions(screen.getByLabelText("Source provider"), "upload");
    const archive = new File(["zip bytes"], "site.zip", {
      type: "application/zip",
    });
    await user.upload(screen.getByLabelText("ZIP archive"), archive);
    await user.click(screen.getByRole("button", { name: "Save source" }));

    await screen.findByText("Source saved");
    const upload = mock.mock.calls.find(
      (call) =>
        (call[1] as RequestInit | undefined)?.method === "POST" &&
        call[0] === "/api/apps/site/source/archive",
    );
    expect(upload?.[1]).toEqual(
      expect.objectContaining({
        body: archive,
        headers: expect.objectContaining({
          "Content-Type": "application/zip",
          "X-Orkano-Filename": "site.zip",
        }),
      }),
    );
    expect(putBody(mock).source).toEqual({
      upload: {
        digest: `sha256:${"a".repeat(64)}`,
        fileName: "site.zip",
      },
    });
  });

  it("clears a selected ZIP when the provider changes", async () => {
    const mock = stubFetchRoutes({
      "GET /api/features": () =>
        jsonResponse(200, features({ "source.zip": true })),
    });
    renderWithSession(<SourceCard app={makeApp({ name: "site" })} />);
    const user = userEvent.setup();

    const zipOption = await screen.findByRole("option", {
      name: "Upload ZIP — unsafe",
    });
    await waitFor(() => {
      expect(zipOption).not.toBeDisabled();
    });
    await user.selectOptions(screen.getByLabelText("Source provider"), "upload");
    await user.upload(
      screen.getByLabelText("ZIP archive"),
      new File(["old"], "old.zip", { type: "application/zip" }),
    );
    await user.selectOptions(screen.getByLabelText("Source provider"), "github");
    await user.selectOptions(screen.getByLabelText("Source provider"), "upload");
    expect((screen.getByLabelText("ZIP archive") as HTMLInputElement).files).toHaveLength(0);
    await user.click(screen.getByRole("button", { name: "Save source" }));

    expect(await screen.findByText("Choose a ZIP archive.")).toBeInTheDocument();
    expect(mock.mock.calls.some((call) => (call[1] as RequestInit | undefined)?.method === "POST")).toBe(false);
  });

  it("rejects ZIP filenames the API schema cannot store", async () => {
    const mock = stubFetchRoutes({
      "GET /api/features": () =>
        jsonResponse(200, features({ "source.zip": true })),
    });
    renderWithSession(<SourceCard app={makeApp({ name: "site" })} />);
    const user = userEvent.setup();

    await waitFor(() => {
      expect(screen.getByRole("option", { name: "Upload ZIP — unsafe" })).not.toBeDisabled();
    });
    await user.selectOptions(screen.getByLabelText("Source provider"), "upload");
    await user.upload(
      screen.getByLabelText("ZIP archive"),
      new File(["zip"], "SITE.ZIP", { type: "application/zip" }),
    );
    await user.click(screen.getByRole("button", { name: "Save source" }));

    expect(await screen.findByText(/lowercase \.zip extension/)).toBeInTheDocument();
    expect(mock.mock.calls.some((call) => (call[1] as RequestInit | undefined)?.method === "POST")).toBe(false);
  });
});
