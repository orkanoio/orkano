import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DeploysCard } from "@/apps/DeploysCard";
import { jsonResponse, renderWithSession, stubFetchRoutes } from "@/test/helpers";

describe("DeploysCard", () => {
  it("lists recorded deploys newest first", async () => {
    stubFetchRoutes({
      "GET /api/apps/web/deploys": () =>
        jsonResponse(200, {
          items: [
            {
              occurredAt: "2026-07-02T09:00:00Z",
              status: "updated",
              buildName: "web-abcdef123456",
            },
            { occurredAt: "2026-07-01T09:00:00Z", status: "created" },
          ],
        }),
    });
    renderWithSession(<DeploysCard appName="web" />);

    expect(await screen.findByText("updated")).toBeInTheDocument();
    expect(screen.getByText("created")).toBeInTheDocument();
    expect(screen.getByText("web-abcdef123456")).toBeInTheDocument();
  });

  it("shows an empty state", async () => {
    stubFetchRoutes({
      "GET /api/apps/web/deploys": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<DeploysCard appName="web" />);

    expect(
      await screen.findByText("No deploys recorded yet."),
    ).toBeInTheDocument();
  });
});
