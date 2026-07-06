import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { makeSecretStore } from "@/test/fixtures";
import {
  jsonResponse,
  renderWithSession,
  requestBody,
  stubFetchRoutes,
} from "@/test/helpers";
import { StoreForm } from "@/vault/StoreForm";

async function fillConnect(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText("Name"), "team-vault");
  await user.type(
    screen.getByLabelText("Vault server"),
    "https://vault.internal.example:8200",
  );
  await user.type(screen.getByLabelText("Vault token"), "s.scoped-token");
}

describe("StoreForm", () => {
  it("connects a store and navigates back", async () => {
    const mock = stubFetchRoutes({
      "POST /api/secretstores": () => jsonResponse(201, makeSecretStore()),
    });
    renderWithSession(<StoreForm />);
    const user = userEvent.setup();

    await fillConnect(user);
    await user.click(screen.getByRole("button", { name: "Connect store" }));

    expect(await requestBody(mock)).toEqual({
      name: "team-vault",
      vault: {
        server: "https://vault.internal.example:8200",
        path: "secret",
        version: "v2",
      },
      token: "s.scoped-token",
    });
    expect(window.location.hash).toBe("#/vault");
  });

  it("refuses reserved names and non-https servers client-side", async () => {
    const mock = stubFetchRoutes({});
    renderWithSession(<StoreForm />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Name"), "acme-credentials");
    await user.type(
      screen.getByLabelText("Vault server"),
      "http://vault.internal:8200",
    );
    await user.type(screen.getByLabelText("Vault token"), "t");
    await user.click(screen.getByRole("button", { name: "Connect store" }));

    expect(await screen.findByText(/endings are reserved/)).toBeInTheDocument();
    expect(screen.getByText(/Must be an https:\/\//)).toBeInTheDocument();
    expect(mock).not.toHaveBeenCalled();
  });

  it("opens the step-up form when the write needs a fresh second factor", async () => {
    stubFetchRoutes({
      "POST /api/secretstores": () =>
        jsonResponse(403, { error: "step_up_required" }),
    });
    renderWithSession(<StoreForm />);
    const user = userEvent.setup();

    await fillConnect(user);
    await user.click(screen.getByRole("button", { name: "Connect store" }));

    expect(
      await screen.findByRole("button", { name: "Confirm identity" }),
    ).toBeInTheDocument();
  });

  it("rotates without a name field; an empty token keeps the credential", async () => {
    const mock = stubFetchRoutes({
      "PUT /api/secretstores/team-vault": () =>
        jsonResponse(200, makeSecretStore()),
    });
    renderWithSession(<StoreForm edit="team-vault" />);
    const user = userEvent.setup();

    expect(screen.queryByLabelText("Name")).not.toBeInTheDocument();
    await user.type(
      screen.getByLabelText("Vault server"),
      "https://vault2.internal.example:8200",
    );
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    expect(await requestBody(mock)).toEqual({
      vault: {
        server: "https://vault2.internal.example:8200",
        path: "secret",
        version: "v2",
      },
      token: "",
    });
  });

  it("maps the credentials-name collision to actionable copy", async () => {
    stubFetchRoutes({
      "POST /api/secretstores": () =>
        jsonResponse(409, { error: "credentials_name_taken" }),
    });
    renderWithSession(<StoreForm />);
    const user = userEvent.setup();

    await fillConnect(user);
    await user.click(screen.getByRole("button", { name: "Connect store" }));

    expect(
      await screen.findByText(/already owns the Secret/),
    ).toBeInTheDocument();
  });
});
