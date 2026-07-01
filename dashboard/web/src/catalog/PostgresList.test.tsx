import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { PostgresList } from "@/catalog/PostgresList";
import { makePostgres, readyCondition } from "@/test/fixtures";
import { jsonResponse, renderWithSession, stubFetchRoutes } from "@/test/helpers";

describe("PostgresList", () => {
  it("renders databases with version, storage, and status", async () => {
    stubFetchRoutes({
      "GET /api/postgres": () =>
        jsonResponse(200, {
          items: [
            makePostgres({
              name: "api-db",
              spec: { version: "17", storageSize: "20Gi" },
              status: { conditions: [readyCondition("True", "Available")] },
            }),
            makePostgres({
              name: "queue-db",
              status: {
                conditions: [readyCondition("False", "Provisioning")],
              },
            }),
          ],
        }),
    });
    renderWithSession(<PostgresList />);

    expect(
      await screen.findByRole("link", { name: "api-db" }),
    ).toHaveAttribute("href", "#/databases/api-db");
    expect(screen.getByText("PostgreSQL 17")).toBeInTheDocument();
    expect(screen.getByText("20Gi")).toBeInTheDocument();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByText("Provisioning")).toBeInTheDocument();
  });

  it("shows an empty state and the create link", async () => {
    stubFetchRoutes({
      "GET /api/postgres": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<PostgresList />);

    expect(await screen.findByText(/No databases yet/)).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "New database" }),
    ).toHaveAttribute("href", "#/databases/new");
  });

  it("surfaces a list failure", async () => {
    stubFetchRoutes({
      "GET /api/postgres": () => jsonResponse(503, { error: "unavailable" }),
    });
    renderWithSession(<PostgresList />);

    expect(
      await screen.findByText(/cluster API is unavailable/),
    ).toBeInTheDocument();
  });
});
