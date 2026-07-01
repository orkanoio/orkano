import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import App from "@/App";
import {
  emptyResponse,
  jsonResponse,
  renderWithClient,
  stubFetch,
  stubFetchRoutes,
} from "@/test/helpers";

const authenticated = {
  state: "authenticated",
  oidcEnabled: false,
  username: "admin",
  oidc: false,
};

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

  it("shows the signed-in shell with navigation and session controls", async () => {
    stubFetchRoutes({
      "GET /api/auth/status": () => jsonResponse(200, authenticated),
      "GET /api/apps": () => jsonResponse(200, { items: [] }),
    });
    renderWithClient(<App />);

    expect(await screen.findByText("admin")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Apps" })).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "Databases" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Re-authenticate" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeInTheDocument();
  });

  it("signs out and re-checks the gate", async () => {
    let statusCalls = 0;
    const mock = stubFetchRoutes({
      "GET /api/auth/status": () =>
        jsonResponse(
          200,
          statusCalls++ === 0
            ? authenticated
            : { state: "needs_login", oidcEnabled: false },
        ),
      "GET /api/apps": () => jsonResponse(200, { items: [] }),
      "POST /api/auth/logout": () => emptyResponse(204),
    });
    renderWithClient(<App />);

    await userEvent.click(
      await screen.findByRole("button", { name: "Sign out" }),
    );

    expect(
      await screen.findByRole("heading", { name: "Sign in to Orkano" }),
    ).toBeInTheDocument();
    expect(mock).toHaveBeenCalledWith(
      "/api/auth/logout",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("re-checks the gate even when logout fails", async () => {
    let statusCalls = 0;
    stubFetchRoutes({
      "GET /api/auth/status": () =>
        jsonResponse(
          200,
          statusCalls++ === 0
            ? authenticated
            : { state: "needs_login", oidcEnabled: false },
        ),
      "GET /api/apps": () => jsonResponse(200, { items: [] }),
      // Logout on a dead session still re-checks the gate.
      "POST /api/auth/logout": () =>
        jsonResponse(401, { error: "unauthorized" }),
    });
    renderWithClient(<App />);

    await userEvent.click(
      await screen.findByRole("button", { name: "Sign out" }),
    );

    expect(
      await screen.findByRole("heading", { name: "Sign in to Orkano" }),
    ).toBeInTheDocument();
  });

  it("settles to the signed-in shell immediately after login", async () => {
    let statusCalls = 0;
    stubFetchRoutes({
      // The invalidation refetch never resolves — the shell can only appear
      // via the optimistic setQueryData, which is exactly the contract pinned
      // here.
      "GET /api/auth/status": () =>
        statusCalls++ === 0
          ? jsonResponse(200, { state: "needs_login", oidcEnabled: false })
          : new Promise<Response>(() => undefined),
      "POST /api/auth/login": () =>
        jsonResponse(200, { state: "totp_required" }),
      "POST /api/auth/login/totp": () =>
        jsonResponse(200, { state: "authenticated", username: "admin" }),
      "GET /api/apps": () => jsonResponse(200, { items: [] }),
    });
    renderWithClient(<App />);
    const user = userEvent.setup();

    await user.type(await screen.findByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "hunter2hunter2");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

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
