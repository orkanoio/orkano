import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { makeExternalSecret, makeSecretStore } from "@/test/fixtures";
import {
  jsonResponse,
  renderWithSession,
  stubFetchRoutes,
} from "@/test/helpers";
import { VaultPage } from "@/vault/VaultPage";

describe("VaultPage", () => {
  it("renders stores and syncs with status", async () => {
    stubFetchRoutes({
      "GET /api/secretstores": () =>
        jsonResponse(200, {
          items: [
            makeSecretStore(),
            makeSecretStore({
              name: "broken-vault",
              ready: "False",
              reason: "InvalidProviderConfig",
            }),
          ],
        }),
      "GET /api/externalsecrets": () =>
        jsonResponse(200, { items: [makeExternalSecret()] }),
    });
    renderWithSession(<VaultPage />);

    // "team-vault" renders twice: the store row and the sync's Store column.
    expect((await screen.findAllByText("team-vault")).length).toBeGreaterThan(
      0,
    );
    expect(screen.getByText("InvalidProviderConfig")).toBeInTheDocument();
    expect(screen.getByText("api-stripe")).toBeInTheDocument();
    expect(screen.getByText("STRIPE_KEY")).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "Connect a store" }),
    ).toHaveAttribute("href", "#/vault/connect");
    expect(screen.getAllByRole("link", { name: "Rotate" })[0]).toHaveAttribute(
      "href",
      "#/vault/connect/team-vault",
    );
  });

  it("shows the opt-in install hint when ESO is absent", async () => {
    stubFetchRoutes({
      "GET /api/secretstores": () =>
        jsonResponse(503, { error: "secrets_vault_not_installed" }),
      "GET /api/externalsecrets": () =>
        jsonResponse(503, { error: "secrets_vault_not_installed" }),
    });
    renderWithSession(<VaultPage />);

    expect(
      await screen.findByText(/orkano init --secrets-vault/),
    ).toBeInTheDocument();
    // Never the generic still-installing copy — this state does not self-heal.
    expect(screen.queryByText(/resolves itself/)).not.toBeInTheDocument();
  });

  it("shows empty states", async () => {
    stubFetchRoutes({
      "GET /api/secretstores": () => jsonResponse(200, { items: [] }),
      "GET /api/externalsecrets": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<VaultPage />);

    expect(
      await screen.findByText(/No stores connected yet/),
    ).toBeInTheDocument();
    expect(screen.getByText(/No synced secrets yet/)).toBeInTheDocument();
  });

  it("renders New sync as a real disabled button with no stores", async () => {
    stubFetchRoutes({
      "GET /api/secretstores": () => jsonResponse(200, { items: [] }),
      "GET /api/externalsecrets": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<VaultPage />);

    // asChild+disabled on a Link is inert (disabled never reaches an <a>),
    // so the no-stores state must render an actual <button disabled>.
    const btn = await screen.findByRole("button", { name: "New sync" });
    expect(btn).toBeDisabled();
    expect(
      screen.queryByRole("link", { name: "New sync" }),
    ).not.toBeInTheDocument();
  });

  it("disconnects a store behind a two-step confirm", async () => {
    let deleted = false;
    stubFetchRoutes({
      "GET /api/secretstores": () =>
        jsonResponse(200, {
          items: deleted ? [] : [makeSecretStore()],
        }),
      "GET /api/externalsecrets": () => jsonResponse(200, { items: [] }),
      "DELETE /api/secretstores/team-vault": () => {
        deleted = true;
        return new Response(null, { status: 204 });
      },
    });
    renderWithSession(<VaultPage />);
    const user = userEvent.setup();

    await user.click(
      await screen.findByRole("button", { name: "Disconnect" }),
    );
    await user.click(
      screen.getByRole("button", { name: "Really disconnect" }),
    );

    expect(await screen.findByText(/No stores connected yet/)).toBeInTheDocument();
  });
});
