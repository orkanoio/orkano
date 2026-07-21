import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { AppList } from "@/apps/AppList";
import { makeApp, readyCondition } from "@/test/fixtures";
import { jsonResponse, renderWithSession, stubFetchRoutes } from "@/test/helpers";

describe("AppList", () => {
  it("renders each app as a compact card: name plus a status dot", async () => {
    stubFetchRoutes({
      "GET /api/apps": () =>
        jsonResponse(200, {
          items: [
            makeApp({
              name: "web",
              status: {
                conditions: [readyCondition("True", "Available")],
                url: "https://web.example.com",
              },
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

    // The status rides the link's OWN accessible name — aria-label
    // short-circuits descendant content, so this is the only spelling a
    // screen reader actually hears.
    const web = await screen.findByRole("link", { name: "web — Ready" });
    expect(web).toHaveAttribute("href", "#/apps/web");
    expect(
      screen.getByRole("link", { name: "worker — WaitingForBuild" }),
    ).toBeInTheDocument();
    // The index deliberately carries no detail beyond name + status: the URL,
    // replica counts, and source moved to the detail page.
    expect(
      screen.queryByText("https://web.example.com"),
    ).not.toBeInTheDocument();
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

  it("explains when Orkano CRDs are not ready", async () => {
    stubFetchRoutes({
      "GET /api/apps": () => jsonResponse(503, { error: "cluster_not_ready" }),
    });
    renderWithSession(<AppList />);

    expect(
      await screen.findByText(/missing Orkano's CRDs/),
    ).toBeInTheDocument();
  });
});
