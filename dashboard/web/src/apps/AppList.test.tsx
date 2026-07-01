import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { AppList } from "@/apps/AppList";
import { makeApp, readyCondition } from "@/test/fixtures";
import { jsonResponse, renderWithSession, stubFetchRoutes } from "@/test/helpers";

describe("AppList", () => {
  it("renders apps with status, url, and replica counts", async () => {
    stubFetchRoutes({
      "GET /api/apps": () =>
        jsonResponse(200, {
          items: [
            makeApp({
              name: "web",
              status: {
                conditions: [readyCondition("True", "Available")],
                url: "https://web.example.com",
                availableReplicas: 2,
              },
              spec: { replicas: 2 },
            }),
            makeApp({
              name: "worker",
              spec: { type: "Worker" },
              status: {
                conditions: [
                  readyCondition("False", "WaitingForBuild", "no build yet"),
                ],
              },
            }),
          ],
        }),
    });
    renderWithSession(<AppList />);

    expect(await screen.findByRole("link", { name: "web" })).toHaveAttribute(
      "href",
      "#/apps/web",
    );
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByText("WaitingForBuild")).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "https://web.example.com" }),
    ).toBeInTheDocument();
    expect(screen.getByText("2/2")).toBeInTheDocument();
    expect(screen.getByText("Worker")).toBeInTheDocument();
  });

  it("shows an empty state and the create link", async () => {
    stubFetchRoutes({
      "GET /api/apps": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<AppList />);

    expect(
      await screen.findByText(/No apps yet/),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "New app" })).toHaveAttribute(
      "href",
      "#/apps/new",
    );
  });

  it("surfaces a list failure", async () => {
    stubFetchRoutes({
      "GET /api/apps": () => jsonResponse(503, { error: "unavailable" }),
    });
    renderWithSession(<AppList />);

    expect(
      await screen.findByText(/cluster API is unavailable/),
    ).toBeInTheDocument();
  });
});
