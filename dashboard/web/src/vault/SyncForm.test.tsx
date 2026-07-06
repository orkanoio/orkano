import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { makeExternalSecret, makeSecretStore } from "@/test/fixtures";
import {
  jsonResponse,
  renderWithSession,
  requestBody,
  stubFetchRoutes,
} from "@/test/helpers";
import { SyncForm } from "@/vault/SyncForm";

describe("SyncForm", () => {
  it("creates a sync against the defaulted first store", async () => {
    const mock = stubFetchRoutes({
      "GET /api/secretstores": () =>
        jsonResponse(200, { items: [makeSecretStore()] }),
      "POST /api/externalsecrets": () =>
        jsonResponse(201, makeExternalSecret()),
    });
    renderWithSession(<SyncForm />);
    const user = userEvent.setup();

    await user.type(await screen.findByLabelText("Name"), "api-stripe");
    await user.type(screen.getByLabelText("Variable name 1"), "STRIPE_KEY");
    await user.type(screen.getByLabelText("Vault path 1"), "apps/api/stripe");
    await user.click(screen.getByRole("button", { name: "Create sync" }));

    // Call 0 is the stores list; the create is the next one.
    expect(await requestBody(mock, 1)).toEqual({
      name: "api-stripe",
      storeName: "team-vault",
      refreshInterval: "1h",
      keys: [{ secretKey: "STRIPE_KEY", remoteKey: "apps/api/stripe" }],
    });
    expect(window.location.hash).toBe("#/vault");
  });

  it("adds and removes key rows", async () => {
    stubFetchRoutes({
      "GET /api/secretstores": () =>
        jsonResponse(200, { items: [makeSecretStore()] }),
    });
    renderWithSession(<SyncForm />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "Add key" }));
    expect(screen.getByLabelText("Variable name 2")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove key 2" }));
    expect(screen.queryByLabelText("Variable name 2")).not.toBeInTheDocument();
    // The last row cannot be removed.
    expect(screen.getByRole("button", { name: "Remove key 1" })).toBeDisabled();
  });

  it("guides to the connect form when no store exists", async () => {
    stubFetchRoutes({
      "GET /api/secretstores": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<SyncForm />);

    expect(
      await screen.findByRole("link", { name: "Connect your vault" }),
    ).toHaveAttribute("href", "#/vault/connect");
    expect(screen.queryByLabelText("Name")).not.toBeInTheDocument();
  });

  it("rejects invalid variable names client-side", async () => {
    const mock = stubFetchRoutes({
      "GET /api/secretstores": () =>
        jsonResponse(200, { items: [makeSecretStore()] }),
    });
    renderWithSession(<SyncForm />);
    const user = userEvent.setup();

    await user.type(await screen.findByLabelText("Name"), "api-stripe");
    await user.type(screen.getByLabelText("Variable name 1"), "my.key");
    await user.type(screen.getByLabelText("Vault path 1"), "apps/x");
    await user.click(screen.getByRole("button", { name: "Create sync" }));

    expect(
      await screen.findByText(/unique variable name/),
    ).toBeInTheDocument();
    // Only the stores list was fetched — the create never fired.
    expect(mock).toHaveBeenCalledTimes(1);
  });
});
