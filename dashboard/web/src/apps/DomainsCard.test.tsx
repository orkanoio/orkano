import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { DomainsCard } from "@/apps/DomainsCard";
import { makeDomain, readyCondition } from "@/test/fixtures";
import {
  emptyResponse,
  jsonResponse,
  renderWithSession,
  requestBody,
  stubFetchRoutes,
} from "@/test/helpers";

describe("DomainsCard", () => {
  it("shows only this app's domains with host-conflict and certificate state", async () => {
    stubFetchRoutes({
      "GET /api/domains": () =>
        jsonResponse(200, {
          items: [
            makeDomain({
              name: "web.example.com",
              spec: { host: "web.example.com", appRef: { name: "web" } },
              status: {
                conditions: [
                  readyCondition("False", "HostConflict", "host already taken"),
                  {
                    type: "CertificateReady",
                    status: "False",
                    reason: "Pending",
                  },
                ],
              },
            }),
            makeDomain({
              name: "other.example.com",
              spec: { host: "other.example.com", appRef: { name: "other" } },
            }),
          ],
        }),
    });
    renderWithSession(<DomainsCard appName="web" />);

    expect(await screen.findByText("web.example.com")).toBeInTheDocument();
    expect(screen.queryByText("other.example.com")).not.toBeInTheDocument();
    expect(screen.getByText("HostConflict")).toBeInTheDocument();
    expect(screen.getByText("certificate: Pending")).toBeInTheDocument();
  });

  it("adds a domain named by its lowercased host", async () => {
    const mock = stubFetchRoutes({
      "GET /api/domains": () => jsonResponse(200, { items: [] }),
      "POST /api/domains": () => jsonResponse(201, makeDomain()),
    });
    renderWithSession(<DomainsCard appName="web" />);
    const user = userEvent.setup();

    await user.type(
      await screen.findByLabelText("Hostname"),
      "App.Example.Com",
    );
    await user.click(screen.getByRole("button", { name: "Add domain" }));

    const posts = mock.mock.calls.filter(
      (c) => (c[1] as RequestInit | undefined)?.method === "POST",
    );
    expect(posts).toHaveLength(1);
    expect(await requestBody(mock, mock.mock.calls.indexOf(posts[0]!))).toEqual(
      {
        name: "app.example.com",
        spec: { host: "app.example.com", appRef: { name: "web" } },
      },
    );
  });

  it("removes a domain after confirm, passing through the step-up gate", async () => {
    let deletes = 0;
    stubFetchRoutes({
      "GET /api/domains": () =>
        jsonResponse(200, { items: [makeDomain()] }),
      "DELETE /api/domains/web.example.com": () =>
        ++deletes === 1
          ? jsonResponse(403, { error: "step_up_required" })
          : emptyResponse(204),
      "POST /api/auth/stepup": () => emptyResponse(204),
    });
    renderWithSession(<DomainsCard appName="web" />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "Remove" }));
    // Removal is destructive (routing + certificate) — an explicit confirm
    // stands before the request.
    expect(deletes).toBe(0);
    await user.click(screen.getByRole("button", { name: "Confirm remove" }));

    expect(
      await screen.findByText("This action needs a fresh identity check."),
    ).toBeInTheDocument();
    await user.type(screen.getByLabelText("Password"), "hunter2hunter2");
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm identity" }));

    await waitFor(() => {
      expect(deletes).toBe(2);
    });
  });

  it("rejects a single-label hostname client-side", async () => {
    const mock = stubFetchRoutes({
      "GET /api/domains": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<DomainsCard appName="web" />);
    const user = userEvent.setup();

    await user.type(await screen.findByLabelText("Hostname"), "localhost");
    await user.click(screen.getByRole("button", { name: "Add domain" }));

    expect(
      await screen.findByText(/Enter a full hostname/),
    ).toBeInTheDocument();
    const posts = mock.mock.calls.filter(
      (c) => (c[1] as RequestInit | undefined)?.method === "POST",
    );
    expect(posts).toHaveLength(0);
  });
});
