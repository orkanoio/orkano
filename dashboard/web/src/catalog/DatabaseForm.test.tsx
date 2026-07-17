import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { DatabaseForm } from "@/catalog/DatabaseForm";
import { makeMongo } from "@/test/fixtures";
import {
  jsonResponse,
  renderWithSession,
  requestBody,
  stubFetchRoutes,
} from "@/test/helpers";

describe("DatabaseForm", () => {
  it("creates MongoDB 8.0 and routes to its detail screen", async () => {
    const mock = stubFetchRoutes({
      "POST /api/mongo": () =>
        jsonResponse(201, makeMongo({ name: "documents" })),
    });
    renderWithSession(<DatabaseForm />);
    const user = userEvent.setup();

    await user.selectOptions(screen.getByLabelText("Engine"), "mongo");
    expect(screen.getByDisplayValue("MongoDB 8.0 (major/LTS)")).toBeDisabled();
    expect(screen.getByText(/MONGODB_URI/)).toBeInTheDocument();
    await user.type(screen.getByLabelText("Name"), "documents");
    await user.click(screen.getByRole("button", { name: "Create database" }));

    expect(await requestBody(mock)).toEqual({
      name: "documents",
      spec: { version: "8.0", storageSize: "10Gi" },
    });
    expect(window.location.hash).toBe("#/databases/mongo/documents");
  });
});
