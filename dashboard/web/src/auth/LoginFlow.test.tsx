import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { LoginFlow } from "@/auth/LoginFlow";
import { jsonResponse, renderWithClient, requestBody, stubFetch } from "@/test/helpers";

async function submitCredentials(username = "admin", password = "hunter2hunter2") {
  const user = userEvent.setup();
  await user.type(screen.getByLabelText("Username"), username);
  await user.type(screen.getByLabelText("Password"), password);
  await user.click(screen.getByRole("button", { name: "Sign in" }));
  return user;
}

describe("LoginFlow", () => {
  it("signs in through both factors", async () => {
    const mock = stubFetch();
    const onAuthenticated = vi.fn();
    renderWithClient(
      <LoginFlow oidcEnabled={false} onAuthenticated={onAuthenticated} />,
    );

    mock.mockResolvedValueOnce(jsonResponse(200, { state: "totp_required" }));
    const user = await submitCredentials();
    expect(await requestBody(mock)).toEqual({
      username: "admin",
      password: "hunter2hunter2",
    });

    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "authenticated", username: "admin" }),
    );
    await user.type(
      await screen.findByLabelText("Authenticator code"),
      "123456",
    );
    await user.click(screen.getByRole("button", { name: "Verify" }));

    expect(await requestBody(mock, 1)).toEqual({ code: "123456" });
    expect(onAuthenticated).toHaveBeenCalledWith("admin");
  });

  it("shows invalid credentials without leaving the first step", async () => {
    const mock = stubFetch();
    renderWithClient(<LoginFlow oidcEnabled={false} onAuthenticated={vi.fn()} />);

    mock.mockResolvedValueOnce(
      jsonResponse(401, { error: "invalid_credentials" }),
    );
    await submitCredentials();

    expect(await screen.findByText("Invalid credentials.")).toBeInTheDocument();
    expect(screen.getByLabelText("Username")).toBeInTheDocument();
  });

  it("explains a locked account", async () => {
    const mock = stubFetch();
    renderWithClient(<LoginFlow oidcEnabled={false} onAuthenticated={vi.fn()} />);

    mock.mockResolvedValueOnce(jsonResponse(423, { error: "account_locked" }));
    await submitCredentials();

    expect(
      await screen.findByText(/Account locked after too many failed attempts/),
    ).toBeInTheDocument();
  });

  it("verifies with a recovery code instead of TOTP", async () => {
    const mock = stubFetch();
    const onAuthenticated = vi.fn();
    renderWithClient(
      <LoginFlow oidcEnabled={false} onAuthenticated={onAuthenticated} />,
    );

    mock.mockResolvedValueOnce(jsonResponse(200, { state: "totp_required" }));
    const user = await submitCredentials();

    await user.click(
      await screen.findByRole("button", { name: "Use a recovery code instead" }),
    );
    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "authenticated", username: "admin" }),
    );
    await user.type(screen.getByLabelText("Recovery code"), "abcd-efgh");
    await user.click(screen.getByRole("button", { name: "Verify" }));

    expect(await requestBody(mock, 1)).toEqual({ recoveryCode: "abcd-efgh" });
    expect(onAuthenticated).toHaveBeenCalledWith("admin");
  });

  it("clears the previous attempt when switching second-factor modes", async () => {
    const mock = stubFetch();
    renderWithClient(<LoginFlow oidcEnabled={false} onAuthenticated={vi.fn()} />);

    mock.mockResolvedValueOnce(jsonResponse(200, { state: "totp_required" }));
    const user = await submitCredentials();

    mock.mockResolvedValueOnce(
      jsonResponse(401, { error: "invalid_credentials" }),
    );
    await user.type(
      await screen.findByLabelText("Authenticator code"),
      "000000",
    );
    await user.click(screen.getByRole("button", { name: "Verify" }));
    expect(await screen.findByText("Invalid credentials.")).toBeInTheDocument();

    // Switching modes drops the stale error…
    await user.click(
      screen.getByRole("button", { name: "Use a recovery code instead" }),
    );
    expect(screen.queryByText("Invalid credentials.")).not.toBeInTheDocument();

    // …and toggling back starts from an empty field, so a fresh code posts.
    await user.click(
      screen.getByRole("button", { name: "Use the authenticator code instead" }),
    );
    const codeInput = screen.getByLabelText("Authenticator code");
    expect(codeInput).toHaveValue("");

    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "authenticated", username: "admin" }),
    );
    await user.type(codeInput, "123456");
    await user.click(screen.getByRole("button", { name: "Verify" }));
    expect(await requestBody(mock, 2)).toEqual({ code: "123456" });
  });

  it("drops back to the first step when the challenge expires", async () => {
    const mock = stubFetch();
    renderWithClient(<LoginFlow oidcEnabled={false} onAuthenticated={vi.fn()} />);

    mock.mockResolvedValueOnce(jsonResponse(200, { state: "totp_required" }));
    const user = await submitCredentials();

    mock.mockResolvedValueOnce(jsonResponse(401, { error: "no_challenge" }));
    await user.type(
      await screen.findByLabelText("Authenticator code"),
      "123456",
    );
    await user.click(screen.getByRole("button", { name: "Verify" }));

    expect(
      await screen.findByText("That sign-in attempt expired — start again."),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Username")).toBeInTheDocument();
  });

  it("offers SSO sign-in only when OIDC is configured", () => {
    const { unmount } = renderWithClient(
      <LoginFlow oidcEnabled={true} onAuthenticated={vi.fn()} />,
    );
    const sso = screen.getByRole("link", { name: "Sign in with SSO" });
    expect(sso).toHaveAttribute("href", "/api/auth/oidc/login");
    unmount();

    renderWithClient(<LoginFlow oidcEnabled={false} onAuthenticated={vi.fn()} />);
    expect(
      screen.queryByRole("link", { name: "Sign in with SSO" }),
    ).not.toBeInTheDocument();
  });
});
