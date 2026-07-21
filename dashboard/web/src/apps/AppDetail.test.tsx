import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { AppDetail } from "@/apps/AppDetail";
import { makeApp, readyCondition } from "@/test/fixtures";
import {
  emptyResponse,
  jsonResponse,
  renderWithSession,
  stubFetchRoutes,
} from "@/test/helpers";

// The detail page mounts the env, domains, deploys, and logs cards — the
// routes below serve all of their mount-time queries (logs streams only on
// demand).
function detailRoutes(overrides?: Record<string, () => Response>) {
  return {
    "GET /api/apps/web": () =>
      jsonResponse(
        200,
        makeApp({
          name: "web",
          status: {
            conditions: [readyCondition("True", "Available")],
            url: "https://web.example.com",
            image:
              "orkano-registry.orkano-system.svc.cluster.local/web@sha256:abc",
            latestBuild: "web-abcdef123456",
            availableReplicas: 1,
          },
        }),
      ),
    "GET /api/domains": () => jsonResponse(200, { items: [] }),
    "GET /api/apps/web/deploys": () => jsonResponse(200, { items: [] }),
    "GET /api/postgres": () => jsonResponse(200, { items: [] }),
    "GET /api/mongo": () => jsonResponse(200, { items: [] }),
    "GET /api/apps/web/logs": () =>
      new Response(
        'data: {"pod":"web-1","line":"ready"}\n\n' +
          'event: eof\ndata: {"reason":"streams ended"}\n\n',
        { status: 200, headers: { "Content-Type": "text/event-stream" } },
      ),
    ...overrides,
  };
}

describe("AppDetail", () => {
  it("renders the overview with status, url, and image", async () => {
    stubFetchRoutes(detailRoutes());
    renderWithSession(<AppDetail name="web" />);

    expect(
      await screen.findByRole("heading", { name: "web" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open app" })).toHaveAttribute(
      "href",
      "https://web.example.com",
    );
    expect(screen.getByText(/web@sha256:abc/)).toBeInTheDocument();
    expect(screen.getByText("web-abcdef123456")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Source" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Edit" })).not.toBeInTheDocument();
  });

  it("explains a missing app", async () => {
    stubFetchRoutes({
      "GET /api/apps/gone": () => jsonResponse(404, { error: "not_found" }),
      "GET /api/domains": () => jsonResponse(200, { items: [] }),
      "GET /api/apps/gone/deploys": () => jsonResponse(200, { items: [] }),
    });
    renderWithSession(<AppDetail name="gone" />);

    expect(
      await screen.findByText(/There is no app named/),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to apps" })).toBeInTheDocument();
  });

  it("opens source management from the app menu", async () => {
    stubFetchRoutes(
      detailRoutes({
        "GET /api/features": () =>
          jsonResponse(200, {
            features: [
              {
                id: "source.git",
                label: "Generic Git",
                description: "Unsafe generic Git source.",
                unsafe: true,
                enabled: false,
              },
              {
                id: "source.zip",
                label: "ZIP upload",
                description: "Unsafe ZIP source.",
                unsafe: true,
                enabled: false,
              },
              {
                id: "build.nixpacks",
                label: "Nixpacks",
                description: "Unsafe automatic build detection.",
                unsafe: true,
                enabled: false,
              },
            ],
          }),
      }),
    );
    renderWithSession(<AppDetail name="web" />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "Source" }));
    expect(
      await screen.findByRole("heading", { name: "Source" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Source provider")).toHaveValue("github");
  });

  it("deletes after confirm, passing through the step-up gate", async () => {
    let deletes = 0;
    const mock = stubFetchRoutes(
      detailRoutes({
        "DELETE /api/apps/web": () =>
          ++deletes === 1
            ? jsonResponse(403, { error: "step_up_required" })
            : emptyResponse(204),
        "POST /api/auth/stepup": () => emptyResponse(204),
      } as Record<string, () => Response>),
    );
    renderWithSession(<AppDetail name="web" />);
    const user = userEvent.setup();

    await user.click(await screen.findByRole("button", { name: "Settings" }));
    await user.click(await screen.findByRole("button", { name: "Delete app" }));
    await user.click(screen.getByRole("button", { name: "Confirm delete" }));

    // The stale second factor opens the re-auth form.
    expect(
      await screen.findByText("This action needs a fresh identity check."),
    ).toBeInTheDocument();
    await user.type(screen.getByLabelText("Password"), "hunter2hunter2");
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm identity" }));

    // The delete retried and succeeded → back to the list.
    await waitFor(() => {
      expect(window.location.hash).toBe("#/apps");
    });
    expect(deletes).toBe(2);
    expect(mock).toHaveBeenCalledWith(
      "/api/auth/stepup",
      expect.objectContaining({ method: "POST" }),
    );
  });
});
