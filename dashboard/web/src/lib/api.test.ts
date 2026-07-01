import { describe, expect, it } from "vitest";

import {
  ApiError,
  fetchAuthStatus,
  loginTotp,
  logout,
  redeemInstallToken,
} from "@/lib/api";
import { emptyResponse, jsonResponse, requestBody, stubFetch } from "@/test/helpers";

describe("api client", () => {
  it("parses the auth status", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "needs_login", oidcEnabled: true }),
    );

    const status = await fetchAuthStatus();

    expect(status).toEqual({ state: "needs_login", oidcEnabled: true });
    expect(mock).toHaveBeenCalledWith("/api/auth/status", {
      headers: { Accept: "application/json" },
    });
  });

  it("maps a JSON error body to an ApiError with the server code", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(jsonResponse(401, { error: "invalid_token" }));

    const err = await redeemInstallToken({
      token: "t",
      username: "admin",
      password: "p",
    }).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 401, code: "invalid_token" });
  });

  it("falls back to a status-only code on a non-JSON error body", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(
      new Response("<html>bad gateway</html>", { status: 502 }),
    );

    const err = await fetchAuthStatus().catch((e: unknown) => e);

    expect(err).toMatchObject({ status: 502, code: "http_502" });
  });

  it("posts exactly one second factor", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(
      jsonResponse(200, { state: "authenticated", username: "admin" }),
    );

    await loginTotp({ recoveryCode: "abcd-efgh" });

    expect(await requestBody(mock)).toEqual({ recoveryCode: "abcd-efgh" });
  });

  it("treats a 204 as success without parsing a body", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(emptyResponse(204));

    await expect(logout()).resolves.toBeUndefined();
  });
});
