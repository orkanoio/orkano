import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { SettingsPage } from "@/settings/SettingsPage";
import { jsonResponse, renderWithSession, stubFetchRoutes } from "@/test/helpers";

function makeNode(overrides?: Partial<Record<string, unknown>>) {
  return {
    name: "server-1",
    roles: ["control-plane", "etcd"],
    ready: true,
    status: "Ready",
    unschedulable: false,
    kubeletVersion: "v1.35.5+k3s1",
    osImage: "Ubuntu 24.04 LTS",
    architecture: "arm64",
    internalIP: "192.0.2.10",
    creationTimestamp: "2026-07-01T10:00:00Z",
    ...overrides,
  };
}

describe("SettingsPage", () => {
  it("lists nodes with status, roles, version, and address", async () => {
    stubFetchRoutes({
      "GET /api/nodes": () =>
        jsonResponse(200, {
          items: [
            makeNode(),
            makeNode({
              name: "worker-1",
              roles: ["worker"],
              ready: false,
              status: "NotReady",
              unschedulable: true,
              internalIP: "192.0.2.11",
            }),
          ],
        }),
    });
    renderWithSession(<SettingsPage />);

    expect(await screen.findByText("server-1")).toBeInTheDocument();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByText("control-plane, etcd")).toBeInTheDocument();
    expect(screen.getAllByText("v1.35.5+k3s1")).toHaveLength(2);
    expect(screen.getByText("192.0.2.10")).toBeInTheDocument();
    // The cordoned NotReady worker is called out, not hidden.
    expect(screen.getByText("NotReady · cordoned")).toBeInTheDocument();
  });

  it("shows the add-node recipe with its caveats", async () => {
    stubFetchRoutes({
      "GET /api/nodes": () => jsonResponse(200, { items: [makeNode()] }),
    });
    renderWithSession(<SettingsPage />);

    expect(await screen.findByText("Add a node")).toBeInTheDocument();
    const recipe = screen.getByText(/--node <existing-server>/);
    expect(recipe).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Copy orkano init/ }),
    ).toBeInTheDocument();
    expect(screen.getByText(/odd counts — one or three/)).toBeInTheDocument();
    expect(screen.getByText(/orkano preflight/)).toBeInTheDocument();
  });

  it("surfaces a nodes list failure", async () => {
    stubFetchRoutes({
      "GET /api/nodes": () => jsonResponse(503, { error: "unavailable" }),
    });
    renderWithSession(<SettingsPage />);

    expect(
      await screen.findByText(/cluster API is unavailable/),
    ).toBeInTheDocument();
  });
});
