import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { StepUpForm } from "@/auth/StepUpForm";
import { emptyResponse, jsonResponse, renderWithClient, requestBody, stubFetch } from "@/test/helpers";

describe("StepUpForm", () => {
  it("sends an OIDC session to the IdP re-auth path", () => {
    renderWithClient(<StepUpForm oidc={true} />);

    expect(
      screen.getByRole("link", { name: "Re-authenticate with SSO" }),
    ).toHaveAttribute("href", "/api/auth/oidc/login?stepup=1");
  });

  it("re-proves password + code for a local admin", async () => {
    const mock = stubFetch();
    const onDone = vi.fn();
    renderWithClient(<StepUpForm oidc={false} onDone={onDone} />);

    mock.mockResolvedValueOnce(emptyResponse(204));
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Password"), "hunter2hunter2");
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm identity" }));

    expect(mock).toHaveBeenCalledWith(
      "/api/auth/stepup",
      expect.objectContaining({ method: "POST" }),
    );
    expect(await requestBody(mock)).toEqual({
      password: "hunter2hunter2",
      code: "123456",
    });
    expect(onDone).toHaveBeenCalled();
  });

  it("surfaces a failed re-auth", async () => {
    const mock = stubFetch();
    renderWithClient(<StepUpForm oidc={false} />);

    mock.mockResolvedValueOnce(
      jsonResponse(401, { error: "invalid_credentials" }),
    );
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Password"), "hunter2hunter2");
    await user.type(screen.getByLabelText("Authenticator code"), "000000");
    await user.click(screen.getByRole("button", { name: "Confirm identity" }));

    expect(await screen.findByText("Invalid credentials.")).toBeInTheDocument();
  });
});
