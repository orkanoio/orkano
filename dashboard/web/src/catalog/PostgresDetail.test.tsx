import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { PostgresDetail } from "@/catalog/PostgresDetail";
import { makePostgres, readyCondition } from "@/test/fixtures";
import {
  emptyResponse,
  jsonResponse,
  renderWithSession,
  stubFetchRoutes,
} from "@/test/helpers";

describe("PostgresDetail", () => {
  it("shows status, connection secret, and provisioning failures", async () => {
    stubFetchRoutes({
      "GET /api/postgres/api-db": () =>
        jsonResponse(
          200,
          makePostgres({
            name: "api-db",
            status: {
              conditions: [
                readyCondition("False", "ProvisionFailed", "cannot shrink"),
              ],
              secretName: "api-db",
            },
          }),
        ),
    });
    renderWithSession(<PostgresDetail name="api-db" />);

    expect(
      await screen.findByRole("heading", { name: "api-db" }),
    ).toBeInTheDocument();
    expect(screen.getByText("ProvisionFailed")).toBeInTheDocument();
    expect(screen.getByText("cannot shrink")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Connection" }));
    // The connect hint names the Secret and the load-bearing uri key.
    expect(screen.getAllByText("uri")).not.toHaveLength(0);
    expect(screen.getByText("password")).toBeInTheDocument();
  });

  it("grows storage carrying the immutable version along", async () => {
    const pg = makePostgres({
      name: "api-db",
      spec: { version: "14", storageSize: "10Gi" },
    });
    const mock = stubFetchRoutes({
      "GET /api/postgres/api-db": () => jsonResponse(200, pg),
      "PUT /api/postgres/api-db": (init) =>
        jsonResponse(200, {
          ...pg,
          spec: (JSON.parse(init?.body as string) as { spec: object }).spec,
        }),
    });
    renderWithSession(<PostgresDetail name="api-db" />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "Storage" }));
    const grow = await screen.findByLabelText("Grow storage");
    await user.clear(grow);
    await user.type(grow, "20Gi");
    await user.click(screen.getByRole("button", { name: "Resize" }));

    await waitFor(() => {
      expect(
        mock.mock.calls.filter(
          (c) => (c[1] as RequestInit | undefined)?.method === "PUT",
        ),
      ).toHaveLength(1);
    });
    const put = mock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === "PUT",
    );
    // version rides along: omitting it would be re-defaulted to "16" by the
    // apiserver and rejected by the immutability rule for a 14 database.
    expect(JSON.parse((put?.[1] as RequestInit).body as string)).toEqual({
      spec: { version: "14", storageSize: "20Gi" },
    });
  });

  it("refuses a shrink client-side", async () => {
    stubFetchRoutes({
      "GET /api/postgres/api-db": () =>
        jsonResponse(200, makePostgres({ name: "api-db" })),
    });
    renderWithSession(<PostgresDetail name="api-db" />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "Storage" }));
    const grow = await screen.findByLabelText("Grow storage");
    await user.clear(grow);
    await user.type(grow, "5Gi");
    await user.click(screen.getByRole("button", { name: "Resize" }));

    expect(
      await screen.findByText(/Storage can only grow — currently 10Gi/),
    ).toBeInTheDocument();
    const puts = (
      globalThis.fetch as unknown as { mock: { calls: unknown[][] } }
    ).mock.calls.filter(
      (c) => (c[1] as RequestInit | undefined)?.method === "PUT",
    );
    expect(puts).toHaveLength(0);
  });

  it("deletes after an explicit data-loss confirm", async () => {
    stubFetchRoutes({
      "GET /api/postgres/api-db": () =>
        jsonResponse(200, makePostgres({ name: "api-db" })),
      "DELETE /api/postgres/api-db": () => emptyResponse(204),
    });
    renderWithSession(<PostgresDetail name="api-db" />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "Danger" }));
    await user.click(
      await screen.findByRole("button", { name: "Delete database" }),
    );
    expect(
      screen.getByText(/Permanently delete/),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Delete data" }));

    await waitFor(() => {
      expect(window.location.hash).toBe("#/databases");
    });
  });

  it("passes the delete through the step-up gate", async () => {
    let deletes = 0;
    stubFetchRoutes({
      "GET /api/postgres/api-db": () =>
        jsonResponse(200, makePostgres({ name: "api-db" })),
      "DELETE /api/postgres/api-db": () =>
        ++deletes === 1
          ? jsonResponse(403, { error: "step_up_required" })
          : emptyResponse(204),
      "POST /api/auth/stepup": () => emptyResponse(204),
    });
    renderWithSession(<PostgresDetail name="api-db" />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "Danger" }));
    await user.click(
      await screen.findByRole("button", { name: "Delete database" }),
    );
    await user.click(screen.getByRole("button", { name: "Delete data" }));

    expect(
      await screen.findByText("This action needs a fresh identity check."),
    ).toBeInTheDocument();
    await user.type(screen.getByLabelText("Password"), "hunter2hunter2");
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm identity" }));

    await waitFor(() => {
      expect(window.location.hash).toBe("#/databases");
    });
    expect(deletes).toBe(2);
  });
});
