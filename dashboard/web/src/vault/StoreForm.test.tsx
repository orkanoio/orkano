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

  it("rotation seeds from the LIVE store and preserves its spec", async () => {
    // A non-default path/version pins the load-bearing behavior: the server
    // replaces the whole spec, so a rotate must never quietly reset these.
    const live = makeSecretStore({ path: "kv-prod", version: "v1" });
    const mock = stubFetchRoutes({
      "GET /api/secretstores": () => jsonResponse(200, { items: [live] }),
      "PUT /api/secretstores/team-vault": () => jsonResponse(200, live),
    });
    renderWithSession(<StoreForm edit="team-vault" />);
    const user = userEvent.setup();

    // The form waits for the live store, then seeds from it.
    expect(await screen.findByLabelText("Mount path")).toHaveValue("kv-prod");
    expect(screen.getByLabelText("KV engine version")).toHaveValue("v1");
    expect(screen.getByLabelText("Vault server")).toHaveValue(
      "https://vault.internal.example:8200",
    );
    expect(screen.queryByLabelText("Name")).not.toBeInTheDocument();

    // Rotate with a new token, leaving the spec untouched.
    await user.type(screen.getByLabelText("New token (optional)"), "s.next");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    // Call 0 is the stores list; the PUT carries the live spec + the token.
    expect(await requestBody(mock, 1)).toEqual({
      vault: {
        server: "https://vault.internal.example:8200",
        path: "kv-prod",
        version: "v1",
      },
      token: "s.next",
    });
  });

  it("rotation of a missing store links back instead of a defaults form", async () => {
    stubFetchRoutes({
      "GET /api/secretstores": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<StoreForm edit="gone-vault" />);

    expect(await screen.findByText(/was not found/)).toBeInTheDocument();
    expect(screen.queryByLabelText("Vault server")).not.toBeInTheDocument();
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
