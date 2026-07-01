import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import App from "@/App";
import { emptyResponse, jsonResponse, renderWithClient, stubFetch } from "@/test/helpers";

describe("App auth gate", () => {
  it("shows the bootstrap screen on needs_bootstrap", async () => {
    stubFetch().mockResolvedValueOnce(
      jsonResponse(200, { state: "needs_bootstrap", oidcEnabled: false }),
    );
    renderWithClient(<App />);

    expect(
      await screen.findByRole("heading", { name: "Set up Orkano" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Install token")).toBeInTheDocument();
  });

  it("shows the login screen on needs_login", async () => {
    stubFetch().mockResolvedValueOnce(
      jsonResponse(200, { state: "needs_login", oidcEnabled: false }),
    );
    renderWithClient(<App />);

    expect(
      await screen.findByRole("heading", { name: "Sign in to Orkano" }),
    ).toBeInTheDocument();
  });

  it("shows the signed-in shell with session controls", async () => {
    stubFetch().mockResolvedValueOnce(
      jsonResponse(200, {
        state: "authenticated",
        oidcEnabled: false,
        username: "admin",
        oidc: false,
      }),
    );
    renderWithClient(<App />);

    expect(await screen.findByText("admin")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Re-authenticate" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeInTheDocument();
  });

  it("signs out and re-checks the gate", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(
      jsonResponse(200, {
        state: "authenticated",
        oidcEnabled: false,
        username: "admin",
        oidc: false,
      }),
    );
    renderWithClient(<App />);
    const user = userEvent.setup();

    mock.mockResolvedValueOnce(emptyResponse(204)); // POST /logout
    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "needs_login", oidcEnabled: false }),
    ); // refetched status
    await user.click(await screen.findByRole("button", { name: "Sign out" }));

    expect(
      await screen.findByRole("heading", { name: "Sign in to Orkano" }),
    ).toBeInTheDocument();
    expect(mock).toHaveBeenCalledWith(
      "/api/auth/logout",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("re-checks the gate even when logout fails", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(
      jsonResponse(200, {
        state: "authenticated",
        oidcEnabled: false,
        username: "admin",
        oidc: false,
      }),
    );
    renderWithClient(<App />);

    mock.mockResolvedValueOnce(jsonResponse(401, { error: "unauthorized" })); // POST /logout on a dead session
    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "needs_login", oidcEnabled: false }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));

    expect(
      await screen.findByRole("heading", { name: "Sign in to Orkano" }),
    ).toBeInTheDocument();
  });

  it("settles to the signed-in shell immediately after login", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "needs_login", oidcEnabled: false }),
    );
    renderWithClient(<App />);
    const user = userEvent.setup();

    mock.mockResolvedValueOnce(jsonResponse(200, { state: "totp_required" }));
    await user.type(await screen.findByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "hunter2hunter2");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "authenticated", username: "admin" }),
    );
    // The invalidation refetch never resolves — the shell can only appear via
    // the optimistic setQueryData, which is exactly the contract pinned here.
    mock.mockImplementationOnce(() => new Promise<Response>(() => undefined));
    await user.type(
      await screen.findByLabelText("Authenticator code"),
      "123456",
    );
    await user.click(screen.getByRole("button", { name: "Verify" }));

    expect(await screen.findByText("admin")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeInTheDocument();
  });

  it("offers retry when the API is unreachable", async () => {
    const mock = stubFetch();
    mock.mockRejectedValueOnce(new TypeError("fetch failed"));
    renderWithClient(<App />);

    expect(
      await screen.findByText("Cannot reach the Orkano API."),
    ).toBeInTheDocument();

    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "needs_login", oidcEnabled: false }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(
      await screen.findByRole("heading", { name: "Sign in to Orkano" }),
    ).toBeInTheDocument();
  });

  it("surfaces and scrubs an sso_error query param", async () => {
    window.history.replaceState(null, "", "/?sso_error=not_allowed");
    stubFetch().mockResolvedValueOnce(
      jsonResponse(200, { state: "needs_login", oidcEnabled: true }),
    );
    renderWithClient(<App />);

    expect(
      await screen.findByText(
        "That identity is not allowed to access this dashboard.",
      ),
    ).toBeInTheDocument();
    expect(window.location.search).toBe("");

    // Dismiss clears the banner.
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(
      screen.queryByText("That identity is not allowed to access this dashboard."),
    ).not.toBeInTheDocument();
  });
});
