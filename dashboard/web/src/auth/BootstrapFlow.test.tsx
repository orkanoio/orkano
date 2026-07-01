import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { BootstrapFlow } from "@/auth/BootstrapFlow";
import { deferred, jsonResponse, renderWithClient, requestBody, stubFetch } from "@/test/helpers";

const otpauthUrl =
  "otpauth://totp/Orkano:admin?algorithm=SHA1&digits=6&issuer=Orkano&period=30&secret=JBSWY3DPEHPK3PXP";

async function submitRedeem(password = "correct-horse-battery", confirm = password) {
  const user = userEvent.setup();
  await user.type(screen.getByLabelText("Install token"), "tok-123");
  await user.type(screen.getByLabelText("Username"), "admin");
  await user.type(screen.getByLabelText("Password"), password);
  await user.type(screen.getByLabelText("Confirm password"), confirm);
  await user.click(screen.getByRole("button", { name: "Create admin account" }));
  return user;
}

describe("BootstrapFlow", () => {
  it("walks redeem → recovery codes → TOTP enrollment → authenticated", async () => {
    const mock = stubFetch();
    const onAuthenticated = vi.fn();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={onAuthenticated} />,
    );

    mock.mockResolvedValueOnce(
      jsonResponse(200, {
        otpauthUrl,
        recoveryCodes: ["aaaa-1111", "bbbb-2222"],
      }),
    );
    const user = await submitRedeem();
    expect(await requestBody(mock)).toEqual({
      token: "tok-123",
      username: "admin",
      password: "correct-horse-battery",
    });

    // Recovery codes are shown once and must be acknowledged.
    expect(await screen.findByText("aaaa-1111")).toBeInTheDocument();
    expect(screen.getByText("bbbb-2222")).toBeInTheDocument();
    await user.click(
      screen.getByRole("button", { name: "I've saved my recovery codes" }),
    );

    // Enrollment: QR code + manual secret + code confirm.
    expect(document.querySelector("svg")).not.toBeNull();
    expect(screen.getByText("JBSWY3DPEHPK3PXP")).toBeInTheDocument();

    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "authenticated", username: "admin" }),
    );
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(
      screen.getByRole("button", { name: "Verify and finish setup" }),
    );

    expect(await requestBody(mock, 1)).toEqual({ code: "123456" });
    expect(onAuthenticated).toHaveBeenCalledWith("admin");
  });

  it("rejects a short password client-side without calling the API", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    await submitRedeem("short", "short");

    expect(
      await screen.findByText("Passwords must be at least 12 characters."),
    ).toBeInTheDocument();
    expect(mock).not.toHaveBeenCalled();
  });

  it("rejects mismatched passwords client-side", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    await submitRedeem("correct-horse-battery", "correct-horse-staple!");

    expect(await screen.findByText("Passwords do not match.")).toBeInTheDocument();
    expect(mock).not.toHaveBeenCalled();
  });

  it("surfaces an invalid install token", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    mock.mockResolvedValueOnce(jsonResponse(401, { error: "invalid_token" }));
    await submitRedeem();

    expect(
      await screen.findByText("That install token is not valid."),
    ).toBeInTheDocument();
  });

  it("stays on enrollment after a wrong code", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    mock.mockResolvedValueOnce(
      jsonResponse(200, { otpauthUrl, recoveryCodes: ["aaaa-1111"] }),
    );
    const user = await submitRedeem();
    await user.click(
      await screen.findByRole("button", { name: "I've saved my recovery codes" }),
    );

    mock.mockResolvedValueOnce(jsonResponse(401, { error: "invalid_code" }));
    await user.type(screen.getByLabelText("Authenticator code"), "000000");
    await user.click(
      screen.getByRole("button", { name: "Verify and finish setup" }),
    );

    expect(
      await screen.findByText(
        "That code is not valid — check your authenticator app.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Authenticator code")).toBeInTheDocument();
  });

  it("copies the recovery codes to the clipboard", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    mock.mockResolvedValueOnce(
      jsonResponse(200, { otpauthUrl, recoveryCodes: ["aaaa-1111", "bbbb-2222"] }),
    );
    const user = await submitRedeem();

    await user.click(await screen.findByRole("button", { name: "Copy codes" }));

    expect(
      await screen.findByRole("button", { name: "Copied" }),
    ).toBeInTheDocument();
    expect(await navigator.clipboard.readText()).toBe("aaaa-1111\nbbbb-2222");
  });

  it("keeps the codes visible when the clipboard refuses", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    mock.mockResolvedValueOnce(
      jsonResponse(200, { otpauthUrl, recoveryCodes: ["aaaa-1111"] }),
    );
    const user = await submitRedeem();

    const copyButton = await screen.findByRole("button", { name: "Copy codes" });
    const writeText = vi
      .spyOn(navigator.clipboard, "writeText")
      .mockRejectedValueOnce(new Error("denied"));
    await user.click(copyButton);

    expect(writeText).toHaveBeenCalledWith("aaaa-1111");
    expect(
      await screen.findByRole("button", { name: "Copy codes" }),
    ).toBeInTheDocument();
    expect(screen.getByText("aaaa-1111")).toBeInTheDocument();
  });

  it("tells a late visitor that setup is already complete", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    mock.mockResolvedValueOnce(
      jsonResponse(409, { error: "already_bootstrapped" }),
    );
    await submitRedeem();

    expect(
      await screen.findByText("Setup is already complete — sign in instead."),
    ).toBeInTheDocument();
  });

  it("surfaces a lost enrollment race on confirm", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    mock.mockResolvedValueOnce(
      jsonResponse(200, { otpauthUrl, recoveryCodes: ["aaaa-1111"] }),
    );
    const user = await submitRedeem();
    await user.click(
      await screen.findByRole("button", { name: "I've saved my recovery codes" }),
    );

    // A concurrent redeem confirmed first (the single-confirmed-admin index).
    mock.mockResolvedValueOnce(
      jsonResponse(409, { error: "already_bootstrapped" }),
    );
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(
      screen.getByRole("button", { name: "Verify and finish setup" }),
    );

    expect(
      await screen.findByText("Setup is already complete — sign in instead."),
    ).toBeInTheDocument();
  });

  it("disables the submit button while redeem is in flight", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    const { promise, resolve } = deferred<Response>();
    mock.mockReturnValueOnce(promise);
    await submitRedeem();

    const pending = screen.getByRole("button", { name: "Creating admin…" });
    expect(pending).toBeDisabled();

    resolve(jsonResponse(200, { otpauthUrl, recoveryCodes: ["aaaa-1111"] }));
    expect(await screen.findByText("aaaa-1111")).toBeInTheDocument();
  });

  it("offers SSO sign-in on the bootstrap screen when OIDC is configured", () => {
    renderWithClient(
      <BootstrapFlow oidcEnabled={true} onAuthenticated={vi.fn()} />,
    );

    expect(
      screen.getByRole("link", { name: "Sign in with SSO" }),
    ).toHaveAttribute("href", "/api/auth/oidc/login");
  });

  it("restarts from redeem when the enrollment window expires", async () => {
    const mock = stubFetch();
    renderWithClient(
      <BootstrapFlow oidcEnabled={false} onAuthenticated={vi.fn()} />,
    );

    mock.mockResolvedValueOnce(
      jsonResponse(200, { otpauthUrl, recoveryCodes: ["aaaa-1111"] }),
    );
    const user = await submitRedeem();
    await user.click(
      await screen.findByRole("button", { name: "I've saved my recovery codes" }),
    );

    mock.mockResolvedValueOnce(jsonResponse(401, { error: "no_challenge" }));
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(
      screen.getByRole("button", { name: "Verify and finish setup" }),
    );

    expect(
      await screen.findByText(
        "The enrollment window expired — redeem the install token again.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Install token")).toBeInTheDocument();
  });
});
