import { describe, expect, it } from "vitest";

import {
  ApiError,
  appLogsPath,
  createApp,
  deletePostgres,
  fetchAuthStatus,
  listApps,
  loginTotp,
  logout,
  redeemInstallToken,
  setAppEnv,
  updateApp,
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

describe("app/catalog client", () => {
  it("unwraps list envelopes", async () => {
    stubFetch().mockResolvedValueOnce(
      jsonResponse(200, { items: [{ name: "web" }] }),
    );

    const apps = await listApps();

    expect(apps).toEqual([{ name: "web" }]);
  });

  it("creates an App with name and spec", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(jsonResponse(201, { name: "web" }));
    const spec = {
      source: { github: { repo: "o/r" } },
      build: { strategy: "Dockerfile" as const },
    };

    await createApp("web", spec);

    expect(mock).toHaveBeenCalledWith(
      "/api/apps",
      expect.objectContaining({ method: "POST" }),
    );
    expect(await requestBody(mock)).toEqual({ name: "web", spec });
  });

  it("updates an App by PUT with an encoded name", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(jsonResponse(200, { name: "my app" }));
    const spec = {
      source: { github: { repo: "o/r" } },
      build: { strategy: "Static" as const, static: { dir: "public" } },
    };

    await updateApp("my app", spec);

    expect(mock).toHaveBeenCalledWith(
      "/api/apps/my%20app",
      expect.objectContaining({ method: "PUT" }),
    );
    expect(await requestBody(mock)).toEqual({ spec });
  });

  it("replaces the secret env set", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(jsonResponse(200, { name: "web" }));

    await setAppEnv("web", { API_KEY: "hunter2" });

    expect(mock).toHaveBeenCalledWith(
      "/api/apps/web/env",
      expect.objectContaining({ method: "PUT" }),
    );
    expect(await requestBody(mock)).toEqual({
      secrets: { API_KEY: "hunter2" },
    });
  });

  it("deletes without parsing a body", async () => {
    const mock = stubFetch();
    mock.mockResolvedValueOnce(emptyResponse(204));

    await expect(deletePostgres("api-db")).resolves.toBeUndefined();
    expect(mock).toHaveBeenCalledWith(
      "/api/postgres/api-db",
      expect.objectContaining({ method: "DELETE" }),
    );
  });

  it("builds the logs stream path", () => {
    expect(appLogsPath("web")).toBe("/api/apps/web/logs");
    expect(appLogsPath("web", { pod: "web-1", follow: false, tail: 50 })).toBe(
      "/api/apps/web/logs?pod=web-1&follow=false&tail=50",
    );
  });
});
