import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { RepoAllowlistEditor } from "@/github/RepoAllowlistEditor";
import {
  emptyResponse,
  jsonResponse,
  renderWithSession,
  requestBody,
  stubFetchRoutes,
} from "@/test/helpers";

describe("RepoAllowlistEditor", () => {
  it("adds an exact repository and adopts the canonical server response", async () => {
    const onDirtyChange = vi.fn();
    const mock = stubFetchRoutes({
      "PUT /api/repo-allowlist": () =>
        jsonResponse(200, {
          repositories: ["acme/api", "orkanoio/orkano"],
          resourceVersion: "8",
        }),
    });
    renderWithSession(
      <RepoAllowlistEditor
        repositories={["OrkanoIO/Orkano"]}
        resourceVersion="7"
        onDirtyChange={onDirtyChange}
      />,
    );
    const user = userEvent.setup();

    await user.click(screen.getByRole("button", { name: "Add repository" }));
    await user.type(screen.getByLabelText("Allowed repository 2"), "acme/api");
    expect(
      onDirtyChange.mock.calls[onDirtyChange.mock.calls.length - 1]?.[0],
    ).toBe(true);
    await user.click(
      screen.getByRole("button", { name: "Save repositories" }),
    );

    await waitFor(() => {
      expect(
        screen.getByText(/Allowed repositories updated/),
      ).toBeInTheDocument();
    });
    expect(await requestBody(mock)).toEqual({
      repositories: ["OrkanoIO/Orkano", "acme/api"],
      resourceVersion: "7",
    });
    expect(screen.getByLabelText("Allowed repository 1")).toHaveValue(
      "acme/api",
    );
    expect(screen.getByLabelText("Allowed repository 2")).toHaveValue(
      "orkanoio/orkano",
    );
    expect(
      onDirtyChange.mock.calls[onDirtyChange.mock.calls.length - 1]?.[0],
    ).toBe(false);
  });

  it("rejects owner-wide and duplicate entries before a request", async () => {
    const mock = stubFetchRoutes({});
    renderWithSession(
      <RepoAllowlistEditor
        repositories={["acme/api"]}
        resourceVersion="7"
      />,
    );
    const user = userEvent.setup();

    await user.click(screen.getByRole("button", { name: "Add repository" }));
    await user.type(screen.getByLabelText("Allowed repository 2"), "acme");
    await user.click(
      screen.getByRole("button", { name: "Save repositories" }),
    );
    expect(
      await screen.findByText(/exact owner\/repository form/),
    ).toBeInTheDocument();

    await user.clear(screen.getByLabelText("Allowed repository 2"));
    await user.type(screen.getByLabelText("Allowed repository 2"), "ACME/API");
    await user.click(
      screen.getByRole("button", { name: "Save repositories" }),
    );
    expect(
      await screen.findByText("This repository is already listed."),
    ).toBeInTheDocument();
    expect(mock).not.toHaveBeenCalled();
  });

  it("retries the same list after a fresh identity check", async () => {
    let puts = 0;
    stubFetchRoutes({
      "PUT /api/repo-allowlist": () =>
        ++puts === 1
          ? jsonResponse(403, { error: "step_up_required" })
          : jsonResponse(200, {
              repositories: ["acme/api"],
              resourceVersion: "8",
            }),
      "POST /api/auth/stepup": () => emptyResponse(204),
    });
    renderWithSession(
      <RepoAllowlistEditor
        repositories={["acme/api"]}
        resourceVersion="7"
      />,
    );
    const user = userEvent.setup();

    await user.click(
      screen.getByRole("button", { name: "Save repositories" }),
    );
    expect(
      await screen.findByText("This action needs a fresh identity check."),
    ).toBeInTheDocument();
    await user.type(screen.getByLabelText("Password"), "hunter2hunter2");
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm identity" }));

    await waitFor(() => {
      expect(
        screen.queryByText("This action needs a fresh identity check."),
      ).not.toBeInTheDocument();
    });
    expect(puts).toBe(2);
  });
});
