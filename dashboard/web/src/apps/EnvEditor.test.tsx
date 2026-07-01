import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { EnvEditor } from "@/apps/EnvEditor";
import type { EnvVar } from "@/lib/api";
import { makeApp } from "@/test/fixtures";
import {
  emptyResponse,
  jsonResponse,
  renderWithSession,
  stubFetchRoutes,
} from "@/test/helpers";

// An app with all three env shapes: a plaintext var, a reference to a foreign
// Secret (a database connection), and a managed "<app>-env" secret ref.
function appWithEnv() {
  return makeApp({
    name: "web",
    spec: {
      source: { github: { repo: "o/r" } },
      build: { strategy: "Dockerfile" },
      env: [
        { name: "MODE", value: "prod" },
        { name: "DATABASE_URL", secretRef: { name: "api-db", key: "uri" } },
        { name: "API_KEY", secretRef: { name: "web-env", key: "API_KEY" } },
      ],
    },
  });
}

describe("EnvEditor variables", () => {
  it("shows plain and foreign-ref rows but not managed secrets", () => {
    stubFetchRoutes({});
    renderWithSession(<EnvEditor app={appWithEnv()} />);

    const names = screen.getAllByLabelText("Variable name");
    expect(names.map((i) => (i as HTMLInputElement).value)).toEqual([
      "MODE",
      "DATABASE_URL",
    ]);
    expect(screen.getByDisplayValue("prod")).toBeInTheDocument();
    expect(screen.getByDisplayValue("api-db")).toBeInTheDocument();
    expect(screen.getByDisplayValue("uri")).toBeInTheDocument();
    // The managed key appears only in the secrets section, without a value.
    expect(
      (screen.getByLabelText("Secret variable name") as HTMLInputElement).value,
    ).toBe("API_KEY");
    expect(
      (screen.getByLabelText("Secret variable value") as HTMLInputElement)
        .value,
    ).toBe("");
  });

  it("saves variables through the spec, preserving managed refs", async () => {
    const app = appWithEnv();
    const mock = stubFetchRoutes({
      "PUT /api/apps/web": (init) =>
        jsonResponse(200, {
          ...app,
          spec: (JSON.parse(init?.body as string) as { spec: object }).spec,
        }),
    });
    renderWithSession(<EnvEditor app={app} />);
    const user = userEvent.setup();

    await user.click(screen.getByRole("button", { name: "Add variable" }));
    const names = screen.getAllByLabelText("Variable name");
    await user.type(names[names.length - 1] as HTMLElement, "DEBUG");
    const values = screen.getAllByLabelText("Variable value");
    await user.type(values[values.length - 1] as HTMLElement, "true");
    await user.click(screen.getByRole("button", { name: "Save variables" }));

    const puts = mock.mock.calls.filter(
      (c) => (c[1] as RequestInit | undefined)?.method === "PUT",
    );
    expect(puts).toHaveLength(1);
    const body = JSON.parse((puts[0]?.[1] as RequestInit).body as string) as {
      spec: { env: unknown };
    };
    expect(body.spec.env).toEqual([
      { name: "MODE", value: "prod" },
      { name: "DATABASE_URL", secretRef: { name: "api-db", key: "uri" } },
      { name: "DEBUG", value: "true" },
      // The managed ref rides along untouched.
      { name: "API_KEY", secretRef: { name: "web-env", key: "API_KEY" } },
    ]);
  });

  it("rejects an invalid variable name client-side", async () => {
    const mock = stubFetchRoutes({});
    renderWithSession(<EnvEditor app={appWithEnv()} />);
    const user = userEvent.setup();

    await user.click(screen.getByRole("button", { name: "Add variable" }));
    const names = screen.getAllByLabelText("Variable name");
    await user.type(names[names.length - 1] as HTMLElement, "1BAD");
    await user.click(screen.getByRole("button", { name: "Save variables" }));

    expect(
      await screen.findByText(/must start with a letter or underscore/),
    ).toBeInTheDocument();
    expect(mock).not.toHaveBeenCalled();
  });

  it("rejects an empty value, an incomplete reference, a reference to the app's own env Secret, and a managed-name collision", async () => {
    const mock = stubFetchRoutes({});
    renderWithSession(<EnvEditor app={appWithEnv()} />);
    const user = userEvent.setup();
    const lastRow = () => {
      const names = screen.getAllByLabelText("Variable name");
      return names.length - 1;
    };
    const save = () => screen.getByRole("button", { name: "Save variables" });

    // Empty value: the server would drop it (omitempty) and CEL 422s.
    await user.click(screen.getByRole("button", { name: "Add variable" }));
    let i = lastRow();
    await user.type(
      screen.getAllByLabelText("Variable name")[i] as HTMLElement,
      "EMPTYVAR",
    );
    await user.click(save());
    expect(await screen.findByText("EMPTYVAR needs a value.")).toBeInTheDocument();

    // Incomplete reference: a ref row needs both Secret name and key.
    await user.selectOptions(
      screen.getAllByLabelText("Variable kind")[i] as HTMLElement,
      "ref",
    );
    await user.click(save());
    expect(
      await screen.findByText("Reference EMPTYVAR needs a Secret name and key."),
    ).toBeInTheDocument();

    // A reference to the app's own managed Secret belongs in the secrets
    // section, not here. (The DATABASE_URL row also renders Secret name/key
    // inputs, so target the last ones.)
    i = lastRow();
    const refNames = screen.getAllByLabelText("Secret name");
    await user.type(refNames[refNames.length - 1] as HTMLElement, "web-env");
    const refKeys = screen.getAllByLabelText("Secret key");
    await user.type(refKeys[refKeys.length - 1] as HTMLElement, "EMPTYVAR");
    await user.click(save());
    expect(
      await screen.findByText(/Use the secret values section below/),
    ).toBeInTheDocument();

    // A name owned by the secrets section cannot be redefined here.
    await user.selectOptions(
      screen.getAllByLabelText("Variable kind")[i] as HTMLElement,
      "value",
    );
    const nameInput = screen.getAllByLabelText("Variable name")[i] as HTMLElement;
    await user.clear(nameInput);
    await user.type(nameInput, "API_KEY");
    await user.type(
      screen.getAllByLabelText("Variable value")[1] as HTMLElement,
      "x",
    );
    await user.click(save());
    expect(
      await screen.findByText(/API_KEY is managed in the secret values section/),
    ).toBeInTheDocument();

    expect(mock).not.toHaveBeenCalled();
  });

  it("enforces the 64-variable cap exactly", async () => {
    // 62 plain vars + 1 managed ref = 63; one added row lands exactly on the
    // 64 cap (save succeeds), a second lands on 65 (refused, no request).
    const env: EnvVar[] = Array.from({ length: 62 }, (_, i) => ({
      name: `V${i.toString()}`,
      value: "x",
    }));
    env.push({
      name: "API_KEY",
      secretRef: { name: "web-env", key: "API_KEY" },
    });
    const app = makeApp({ name: "web", spec: { env } });
    let puts = 0;
    stubFetchRoutes({
      "PUT /api/apps/web": () => {
        puts++;
        return jsonResponse(200, app);
      },
    });
    renderWithSession(<EnvEditor app={app} />);
    const user = userEvent.setup();
    const save = () => screen.getByRole("button", { name: "Save variables" });

    await user.click(screen.getByRole("button", { name: "Add variable" }));
    let names = screen.getAllByLabelText("Variable name");
    await user.type(names[names.length - 1] as HTMLElement, "AT_CAP");
    let values = screen.getAllByLabelText("Variable value");
    await user.type(values[values.length - 1] as HTMLElement, "ok");
    await user.click(save());
    await waitFor(() => {
      expect(puts).toBe(1);
    });

    await user.click(screen.getByRole("button", { name: "Add variable" }));
    names = screen.getAllByLabelText("Variable name");
    await user.type(names[names.length - 1] as HTMLElement, "OVER_CAP");
    values = screen.getAllByLabelText("Variable value");
    await user.type(values[values.length - 1] as HTMLElement, "no");
    await user.click(save());
    expect(
      await screen.findByText(/at most 64 environment variables/),
    ).toBeInTheDocument();
    expect(puts).toBe(1);
  });
});

describe("EnvEditor secrets", () => {
  it("replaces the whole secret set value-blind", async () => {
    const app = appWithEnv();
    const mock = stubFetchRoutes({
      "PUT /api/apps/web/env": () => jsonResponse(200, app),
    });
    renderWithSession(<EnvEditor app={app} />);
    const user = userEvent.setup();

    await user.type(
      screen.getByLabelText("Secret variable value"),
      "rotated-value",
    );
    await user.click(screen.getByRole("button", { name: "Add secret" }));
    const keys = screen.getAllByLabelText("Secret variable name");
    await user.type(keys[keys.length - 1] as HTMLElement, "TOKEN");
    const values = screen.getAllByLabelText("Secret variable value");
    await user.type(values[values.length - 1] as HTMLElement, "t0ken");
    await user.click(screen.getByRole("button", { name: "Save secrets" }));

    const puts = mock.mock.calls.filter(
      (c) => (c[1] as RequestInit | undefined)?.method === "PUT",
    );
    expect(puts).toHaveLength(1);
    expect(
      JSON.parse((puts[0]?.[1] as RequestInit).body as string),
    ).toEqual({
      secrets: { API_KEY: "rotated-value", TOKEN: "t0ken" },
    });
  });

  it("opens the step-up gate on 403 and retries after confirmation", async () => {
    const app = appWithEnv();
    let envPuts = 0;
    stubFetchRoutes({
      "PUT /api/apps/web/env": () =>
        ++envPuts === 1
          ? jsonResponse(403, { error: "step_up_required" })
          : jsonResponse(200, app),
      "POST /api/auth/stepup": () => emptyResponse(204),
    });
    renderWithSession(<EnvEditor app={app} />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Secret variable value"), "v");
    await user.click(screen.getByRole("button", { name: "Save secrets" }));

    expect(
      await screen.findByText("This action needs a fresh identity check."),
    ).toBeInTheDocument();
    await user.type(screen.getByLabelText("Password"), "hunter2hunter2");
    await user.type(screen.getByLabelText("Authenticator code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm identity" }));

    // The env write retried with the same payload and the gate closed.
    await waitFor(() => {
      expect(
        screen.queryByText("This action needs a fresh identity check."),
      ).not.toBeInTheDocument();
    });
    expect(envPuts).toBe(2);
  });
});
