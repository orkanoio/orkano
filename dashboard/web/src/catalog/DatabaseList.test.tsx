import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DatabaseList } from "@/catalog/DatabaseList";
import { makeMongo, makePostgres, readyCondition } from "@/test/fixtures";
import { jsonResponse, renderWithSession, stubFetchRoutes } from "@/test/helpers";

describe("DatabaseList", () => {
  it("combines PostgreSQL and MongoDB in one catalog", async () => {
    stubFetchRoutes({
      "GET /api/postgres": () =>
        jsonResponse(200, {
          items: [
            makePostgres({
              name: "accounts",
              status: { conditions: [readyCondition("True", "Available")] },
            }),
          ],
        }),
      "GET /api/mongo": () =>
        jsonResponse(200, {
          items: [makeMongo({ name: "documents" })],
        }),
    });
    renderWithSession(<DatabaseList />);

    expect(await screen.findByText("PostgreSQL 16")).toBeInTheDocument();
    expect(screen.getByText("MongoDB 8.0")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "accounts" })).toHaveAttribute(
      "href",
      "#/databases/accounts",
    );
    expect(screen.getByRole("link", { name: "documents" })).toHaveAttribute(
      "href",
      "#/databases/mongo/documents",
    );
  });

  it("waits for both engines before showing the empty state", async () => {
    stubFetchRoutes({
      "GET /api/postgres": () => jsonResponse(200, { items: [] }),
      "GET /api/mongo": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<DatabaseList />);

    expect(await screen.findByText(/No databases yet/)).toBeInTheDocument();
  });
});
