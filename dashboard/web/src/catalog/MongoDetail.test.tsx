import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { MongoDetail } from "@/catalog/MongoDetail";
import { makeMongo, readyCondition } from "@/test/fixtures";
import {
  jsonResponse,
  renderWithSession,
  stubFetchRoutes,
} from "@/test/helpers";

describe("MongoDetail", () => {
  it("shows the MONGODB_URI Secret contract and readiness", async () => {
    stubFetchRoutes({
      "GET /api/mongo/documents": () =>
        jsonResponse(
          200,
          makeMongo({
            name: "documents",
            status: {
              secretName: "documents",
              conditions: [readyCondition("True", "Available")],
            },
          }),
        ),
    });
    renderWithSession(<MongoDetail name="documents" />);

    expect(await screen.findByText("Ready")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Connection" }));
    expect(await screen.findByText("MONGODB_URI")).toBeInTheDocument();
    expect(screen.getByText("password")).toBeInTheDocument();
  });

  it("grows storage while carrying the immutable 8.0 version", async () => {
    const mongo = makeMongo({ name: "documents" });
    const mock = stubFetchRoutes({
      "GET /api/mongo/documents": () => jsonResponse(200, mongo),
      "PUT /api/mongo/documents": (init) =>
        jsonResponse(200, {
          ...mongo,
          spec: (JSON.parse(init?.body as string) as { spec: object }).spec,
        }),
    });
    renderWithSession(<MongoDetail name="documents" />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "Storage" }));
    const input = await screen.findByLabelText("Grow storage");
    await user.clear(input);
    await user.type(input, "20Gi");
    await user.click(screen.getByRole("button", { name: "Resize" }));

    await waitFor(() => {
      expect(
        mock.mock.calls.filter(
          (call) => (call[1] as RequestInit | undefined)?.method === "PUT",
        ),
      ).toHaveLength(1);
    });
    const put = mock.mock.calls.find(
      (call) => (call[1] as RequestInit | undefined)?.method === "PUT",
    );
    expect(JSON.parse((put?.[1] as RequestInit).body as string)).toEqual({
      spec: { version: "8.0", storageSize: "20Gi" },
    });
  });
});
