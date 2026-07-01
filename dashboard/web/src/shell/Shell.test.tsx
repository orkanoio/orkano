import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { makePostgres } from "@/test/fixtures";
import { jsonResponse, renderWithClient, stubFetchRoutes } from "@/test/helpers";

import { Shell } from "./Shell";

const status = {
  state: "authenticated" as const,
  oidcEnabled: false,
  username: "admin",
  oidc: false,
};

describe("Shell routing", () => {
  it("routes hash paths to the right screens", async () => {
    stubFetchRoutes({
      "GET /api/apps": () => jsonResponse(200, { items: [] }),
      "GET /api/postgres": () =>
        jsonResponse(200, { items: [makePostgres({ name: "api-db" })] }),
    });
    renderWithClient(<Shell status={status} />);
    const user = userEvent.setup();

    // Default route is the app list.
    expect(
      await screen.findByRole("heading", { name: "Apps" }),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("link", { name: "Databases" }));
    expect(
      await screen.findByRole("heading", { name: "Databases" }),
    ).toBeInTheDocument();
    expect(await screen.findByText("api-db")).toBeInTheDocument();

    await user.click(screen.getByRole("link", { name: "New database" }));
    expect(
      await screen.findByRole("heading", { name: "New database" }),
    ).toBeInTheDocument();
  });

  it("renders a not-found route with a way back", async () => {
    window.location.hash = "/nonsense";
    stubFetchRoutes({});
    renderWithClient(<Shell status={status} />);

    expect(await screen.findByText(/Nothing here/)).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "back to apps" }),
    ).toBeInTheDocument();
  });

  it("shows the session identity with the SSO marker", async () => {
    stubFetchRoutes({
      "GET /api/apps": () => jsonResponse(200, { items: [] }),
    });
    renderWithClient(
      <Shell status={{ ...status, username: "dev@example.com", oidc: true }} />,
    );

    expect(await screen.findByText("dev@example.com")).toBeInTheDocument();
    expect(screen.getByText(/via SSO/)).toBeInTheDocument();
  });
});
