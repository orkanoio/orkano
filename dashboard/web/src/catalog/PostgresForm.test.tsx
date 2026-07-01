import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { PostgresForm } from "@/catalog/PostgresForm";
import { makePostgres } from "@/test/fixtures";
import {
  jsonResponse,
  renderWithSession,
  requestBody,
  stubFetchRoutes,
} from "@/test/helpers";

describe("PostgresForm", () => {
  it("creates a database and navigates to it", async () => {
    const mock = stubFetchRoutes({
      "POST /api/postgres": () =>
        jsonResponse(201, makePostgres({ name: "api-db" })),
    });
    renderWithSession(<PostgresForm />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Name"), "api-db");
    await user.selectOptions(screen.getByLabelText("Version"), "17");
    const storage = screen.getByLabelText("Storage");
    await user.clear(storage);
    await user.type(storage, "20Gi");
    await user.click(screen.getByRole("button", { name: "Create database" }));

    expect(await requestBody(mock)).toEqual({
      name: "api-db",
      spec: { version: "17", storageSize: "20Gi" },
    });
    expect(window.location.hash).toBe("#/databases/api-db");
  });

  it("enforces the stricter DNS-1035 name rule client-side", async () => {
    const mock = stubFetchRoutes({});
    renderWithSession(<PostgresForm />);
    const user = userEvent.setup();

    // "1db" passes the server's DNS-1123 check but would stick at
    // ProvisionFailed — the form catches it up front.
    await user.type(screen.getByLabelText("Name"), "1db");
    await user.click(screen.getByRole("button", { name: "Create database" }));

    expect(
      await screen.findByText(/Start with a letter/),
    ).toBeInTheDocument();
    expect(mock).not.toHaveBeenCalled();
  });

  it("enforces the 1Gi storage floor", async () => {
    const mock = stubFetchRoutes({});
    renderWithSession(<PostgresForm />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Name"), "api-db");
    const storage = screen.getByLabelText("Storage");
    await user.clear(storage);
    await user.type(storage, "512Mi");
    await user.click(screen.getByRole("button", { name: "Create database" }));

    expect(await screen.findByText("At least 1Gi.")).toBeInTheDocument();
    expect(mock).not.toHaveBeenCalled();
  });
});
