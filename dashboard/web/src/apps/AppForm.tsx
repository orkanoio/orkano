import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Field } from "@/components/Field";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  appKey,
  appsKey,
  createApp,
  featuresKey,
  fetchFeatures,
  getApp,
  sourceKind,
  updateApp,
  type AppResponse,
  type AppSpec,
  type BuildStrategy,
  type FeatureStatus,
  type SourceKind,
} from "@/lib/api";
import { Link, navigate } from "@/lib/router";

import { UnsafeFeatureNotice } from "./SourceCard";

// Client-side mirrors of the CRD constraints (api/v1alpha1): the apiserver's
// 422 carries only a stable `invalid` code, so mirroring is the only way to
// tell the user WHICH field is wrong.
const nameRe = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;
const repoRe = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const pathRe = /^[A-Za-z0-9_./-]+$/;

interface Fields {
  name: string;
  type: "Web" | "Worker";
  sourceKind: SourceKind;
  repo: string;
  gitURL: string;
  ref: string;
  subPath: string;
  strategy: BuildStrategy["strategy"];
  dockerfilePath: string;
  staticDir: string;
  port: string;
  replicas: string;
  healthPath: string;
}

function fieldsFromApp(app?: AppResponse): Fields {
  const spec = app?.spec;
  const kind = spec ? sourceKind(spec.source) : "github";
  const repo = spec && "github" in spec.source ? spec.source.github.repo : "";
  const gitURL = spec && "git" in spec.source ? spec.source.git.url : "";
  const ref =
    spec && "github" in spec.source
      ? (spec.source.github.ref ?? "")
      : spec && "git" in spec.source
        ? (spec.source.git.ref ?? "")
        : "";
  return {
    name: app?.name ?? "",
    type: spec?.type ?? "Web",
    sourceKind: kind,
    repo,
    gitURL,
    ref,
    subPath: spec?.source.subPath ?? "",
    strategy: spec?.build.strategy ?? "Dockerfile",
    dockerfilePath: spec?.build.dockerfile?.path ?? "",
    staticDir: spec?.build.static?.dir ?? "",
    port: spec?.port?.toString() ?? "",
    replicas: spec?.replicas?.toString() ?? "1",
    healthPath: spec?.healthCheck?.path ?? "",
  };
}

function validPublicGitURL(value: string): boolean {
  try {
    const url = new URL(value);
    return (
      url.protocol === "https:" &&
      url.hostname !== "" &&
      (url.port === "" || url.port === "443") &&
      url.pathname !== "/" &&
      url.username === "" &&
      url.password === "" &&
      url.search === "" &&
      url.hash === ""
    );
  } catch {
    return false;
  }
}

function validate(
  f: Fields,
  creating: boolean,
  featureEnabled: (id: FeatureStatus["id"]) => boolean,
): Record<string, string> {
  const errs: Record<string, string> = {};
  if (creating) {
    // 249 rather than the object-name 253: the env editor derives "<name>-env".
    if (f.name.length > 249 || !nameRe.test(f.name)) {
      errs.name =
        "Use lowercase letters, digits, and hyphens (at most 249 characters).";
    }
  }
  const pathErr = "Use letters, digits, ., _, / and - (no '..').";
  if (creating) {
    if (f.sourceKind === "github") {
      if (f.repo.length > 140 || !repoRe.test(f.repo)) {
        errs.repo = "Use the owner/repository form, e.g. orkanoio/orkano.";
      }
    } else if (f.sourceKind === "git") {
      if (!featureEnabled("source.git")) {
        errs.sourceKind = "Generic Git is not enabled for this installation.";
      } else if (f.gitURL.length > 2048 || !validPublicGitURL(f.gitURL)) {
        errs.gitURL =
          "Use a public HTTPS repository URL without credentials, a query, or a fragment.";
      }
    } else {
      errs.sourceKind = "Create the app first, then upload its archive from Source.";
    }
    if (
      f.ref !== "" &&
      (f.ref.length > 250 || !pathRe.test(f.ref) || f.ref.includes(".."))
    ) {
      errs.ref = "Use letters, digits, ., _, / and - (no '..').";
    }
    if (
      f.subPath !== "" &&
      (f.subPath.length > 512 ||
        !pathRe.test(f.subPath) ||
        f.subPath.includes(".."))
    ) {
      errs.subPath = pathErr;
    }
    if (f.strategy === "Dockerfile") {
      if (
        f.dockerfilePath !== "" &&
        (f.dockerfilePath.length > 512 ||
          !pathRe.test(f.dockerfilePath) ||
          f.dockerfilePath.includes(".."))
      ) {
        errs.dockerfilePath = pathErr;
      }
    } else if (f.strategy === "Static") {
      if (
        f.staticDir === "" ||
        f.staticDir.length > 512 ||
        !pathRe.test(f.staticDir) ||
        f.staticDir.includes("..")
      ) {
        errs.staticDir =
          f.staticDir === "" ? "The directory to serve is required." : pathErr;
      }
    } else if (!featureEnabled("build.nixpacks")) {
      errs.strategy = "Nixpacks is not enabled for this installation.";
    }
  }
  if (f.type === "Web" && f.port !== "") {
    const port = Number(f.port);
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      errs.port = "A port between 1 and 65535.";
    }
  }
  const replicas = Number(f.replicas);
  if (!Number.isInteger(replicas) || replicas < 0 || replicas > 20) {
    errs.replicas = "Between 0 and 20 replicas.";
  }
  if (
    f.type === "Web" &&
    f.healthPath !== "" &&
    (!f.healthPath.startsWith("/") || f.healthPath.length > 512)
  ) {
    errs.healthPath = "A path starting with /.";
  }
  return errs;
}

// buildSpec assembles the full AppSpec, preserving base fields this form does
// not model (env, command, resources) — PUT is whole-spec replacement. The
// wrong-strategy build member and a Worker's port/healthCheck are OMITTED
// entirely (CEL rejects their presence, even empty).
function buildSpec(f: Fields, base?: AppSpec): AppSpec {
  let spec: AppSpec;
  if (base) {
    spec = {
      ...base,
      type: f.type,
      replicas: Number(f.replicas),
    };
  } else {
    const source =
      f.sourceKind === "github"
        ? {
            github: {
              repo: f.repo.trim(),
              ...(f.ref.trim() !== "" && { ref: f.ref.trim() }),
            },
            ...(f.subPath.trim() !== "" && {
              subPath: f.subPath.trim(),
            }),
          }
        : {
            git: {
              url: f.gitURL.trim(),
              ...(f.ref.trim() !== "" && { ref: f.ref.trim() }),
            },
            ...(f.subPath.trim() !== "" && {
              subPath: f.subPath.trim(),
            }),
          };
    const build: BuildStrategy =
      f.strategy === "Dockerfile"
        ? {
            strategy: "Dockerfile",
            ...(f.dockerfilePath.trim() !== "" && {
              dockerfile: { path: f.dockerfilePath.trim() },
            }),
          }
        : f.strategy === "Static"
          ? { strategy: "Static", static: { dir: f.staticDir.trim() } }
          : { strategy: "Nixpacks", nixpacks: {} };
    spec = {
      source,
      build,
      type: f.type,
      replicas: Number(f.replicas),
    };
  }
  delete spec.port;
  delete spec.healthCheck;
  if (f.type === "Web") {
    if (f.port !== "") {
      spec.port = Number(f.port);
    }
    if (f.healthPath !== "") {
      spec.healthCheck = { path: f.healthPath };
    }
  }
  return spec;
}

// AppForm creates an App, or — when edit is set — loads it and replaces its
// spec (round-tripping the fields the form does not model).
export function AppForm({ edit }: { edit?: string }) {
  const query = useQuery({
    queryKey: appKey(edit ?? ""),
    queryFn: () => getApp(edit ?? ""),
    enabled: edit !== undefined,
  });

  if (edit !== undefined) {
    if (query.isPending) {
      return (
        <p className="font-mono text-xs text-muted-foreground">Loading…</p>
      );
    }
    if (query.error) {
      return <ApiErrorAlert error={query.error} />;
    }
    return <AppFormInner base={query.data} />;
  }
  return <AppFormInner />;
}

function AppFormInner({ base }: { base?: AppResponse }) {
  const queryClient = useQueryClient();
  const [fields, setFields] = useState<Fields>(() => fieldsFromApp(base));
  const [errors, setErrors] = useState<Record<string, string>>({});
  const featureQuery = useQuery({
    queryKey: featuresKey,
    queryFn: fetchFeatures,
    enabled: base === undefined,
    staleTime: 60_000,
  });
  const featureEnabled = (id: FeatureStatus["id"]) =>
    featureQuery.data?.some((feature) => feature.id === id && feature.enabled) ??
    false;

  const set = (patch: Partial<Fields>) => {
    setFields((f) => ({ ...f, ...patch }));
  };

  const save = useMutation({
    mutationFn: () => {
      const spec = buildSpec(fields, base?.spec);
      return base
        ? updateApp(base.name, spec)
        : createApp(fields.name.trim(), spec);
    },
    onSuccess: (app) => {
      queryClient.setQueryData(appKey(app.name), app);
      void queryClient.invalidateQueries({ queryKey: appsKey });
      navigate(`/apps/${encodeURIComponent(app.name)}`);
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const errs = validate(
      {
        ...fields,
        name: fields.name.trim(),
        repo: fields.repo.trim(),
        gitURL: fields.gitURL.trim(),
      },
      !base,
      featureEnabled,
    );
    setErrors(errs);
    if (Object.keys(errs).length === 0) {
      save.mutate();
    }
  };

  return (
    <section className="flex max-w-xl flex-col gap-6">
      <h1 className="font-display text-2xl font-medium tracking-tight text-white">
        {base ? `Runtime settings · ${base.name}` : "New app"}
      </h1>
      <form className="flex flex-col gap-6" onSubmit={submit}>
        <ApiErrorAlert error={featureQuery.error} />
        <ApiErrorAlert error={save.error} />
        <Card>
          <CardContent className="flex flex-col gap-4">
            {!base && (
              <Field
                id="app-name"
                label="Name"
                error={errors.name}
                hint="Lowercase letters, digits, and hyphens."
              >
                <Input
                  id="app-name"
                  value={fields.name}
                  onChange={(e) => {
                    set({ name: e.target.value });
                  }}
                  autoFocus
                  required
                />
              </Field>
            )}
            <Field id="app-type" label="Type" error={errors.type}>
              <Select
                id="app-type"
                value={fields.type}
                onChange={(e) => {
                  set({ type: e.target.value as Fields["type"] });
                }}
              >
                <option value="Web">Web — serves HTTP</option>
                <option value="Worker">Worker — background process</option>
              </Select>
            </Field>
            {!base ? (
              <>
                <Field
                  id="app-source-kind"
                  label="Source provider"
                  error={errors.sourceKind}
                  hint="ZIP uploads become available from the app's Source section after creation."
                >
                  <Select
                    id="app-source-kind"
                    value={fields.sourceKind}
                    onChange={(e) => {
                      set({ sourceKind: e.target.value as SourceKind });
                      setErrors({});
                    }}
                  >
                    <option value="github">GitHub App</option>
                    <option
                      value="git"
                      disabled={!featureEnabled("source.git")}
                    >
                      Generic Git — unsafe
                      {featureEnabled("source.git") ? "" : " (disabled)"}
                    </option>
                    <option value="upload" disabled>
                      Upload ZIP — create app first
                    </option>
                  </Select>
                </Field>
                {fields.sourceKind === "github" ? (
                  <Field
                    id="app-repo"
                    label="GitHub repository"
                    error={errors.repo}
                    hint="owner/repository"
                  >
                    <Input
                      id="app-repo"
                      value={fields.repo}
                      onChange={(e) => {
                        set({ repo: e.target.value });
                      }}
                      placeholder="orkanoio/example"
                      required
                    />
                  </Field>
                ) : null}
                {fields.sourceKind === "git" ? (
                  <Field
                    id="app-git-url"
                    label="Public Git URL"
                    error={errors.gitURL}
                    hint="HTTPS only. Credentials, query parameters, and URL fragments are rejected."
                  >
                    <Input
                      id="app-git-url"
                      type="url"
                      value={fields.gitURL}
                      onChange={(e) => {
                        set({ gitURL: e.target.value });
                      }}
                      placeholder="https://git.example.com/team/app.git"
                      required
                    />
                  </Field>
                ) : null}
                <Field
                  id="app-ref"
                  label="Branch or tag"
                  error={errors.ref}
                  hint="Empty means the repository's default branch."
                >
                  <Input
                    id="app-ref"
                    value={fields.ref}
                    onChange={(e) => {
                      set({ ref: e.target.value });
                    }}
                  />
                </Field>
                <Field
                  id="app-subpath"
                  label="Subdirectory"
                  error={errors.subPath}
                  hint="Builds from this directory of the repository (monorepos)."
                >
                  <Input
                    id="app-subpath"
                    value={fields.subPath}
                    onChange={(e) => {
                      set({ subPath: e.target.value });
                    }}
                  />
                </Field>
                <Field id="app-strategy" label="Build" error={errors.strategy}>
                  <Select
                    id="app-strategy"
                    value={fields.strategy}
                    onChange={(e) => {
                      set({ strategy: e.target.value as Fields["strategy"] });
                      setErrors({});
                    }}
                  >
                    <option value="Dockerfile">Dockerfile</option>
                    <option value="Static">Static site</option>
                    <option
                      value="Nixpacks"
                      disabled={!featureEnabled("build.nixpacks")}
                    >
                      Nixpacks — unsafe
                      {featureEnabled("build.nixpacks")
                        ? ""
                        : " (disabled)"}
                    </option>
                  </Select>
                </Field>
                {fields.strategy === "Dockerfile" ? (
                  <Field
                    id="app-dockerfile"
                    label="Dockerfile path"
                    error={errors.dockerfilePath}
                    hint={'Empty means "Dockerfile".'}
                  >
                    <Input
                      id="app-dockerfile"
                      value={fields.dockerfilePath}
                      onChange={(e) => {
                        set({ dockerfilePath: e.target.value });
                      }}
                    />
                  </Field>
                ) : null}
                {fields.strategy === "Static" ? (
                  <Field
                    id="app-staticdir"
                    label="Directory to serve"
                    error={errors.staticDir}
                    hint="The built output directory, e.g. public or dist."
                  >
                    <Input
                      id="app-staticdir"
                      value={fields.staticDir}
                      onChange={(e) => {
                        set({ staticDir: e.target.value });
                      }}
                      required
                    />
                  </Field>
                ) : null}
                {fields.sourceKind === "git"
                  ? featureQuery.data
                      ?.filter((feature) => feature.id === "source.git")
                      .map((feature) => (
                        <UnsafeFeatureNotice
                          key={feature.id}
                          feature={feature}
                        />
                      ))
                  : null}
                {fields.strategy === "Nixpacks"
                  ? featureQuery.data
                      ?.filter((feature) => feature.id === "build.nixpacks")
                      .map((feature) => (
                        <UnsafeFeatureNotice
                          key={feature.id}
                          feature={feature}
                        />
                      ))
                  : null}
              </>
            ) : null}
            {fields.type === "Web" && (
              <Field
                id="app-port"
                label="Port"
                error={errors.port}
                hint="Empty means 8080 (a PORT env var is injected to match)."
              >
                <Input
                  id="app-port"
                  inputMode="numeric"
                  value={fields.port}
                  onChange={(e) => {
                    set({ port: e.target.value });
                  }}
                />
              </Field>
            )}
            <Field id="app-replicas" label="Replicas" error={errors.replicas}>
              <Input
                id="app-replicas"
                inputMode="numeric"
                value={fields.replicas}
                onChange={(e) => {
                  set({ replicas: e.target.value });
                }}
                required
              />
            </Field>
            {fields.type === "Web" && (
              <Field
                id="app-health"
                label="Health check path"
                error={errors.healthPath}
                hint="Empty means a TCP check on the port."
              >
                <Input
                  id="app-health"
                  value={fields.healthPath}
                  onChange={(e) => {
                    set({ healthPath: e.target.value });
                  }}
                  placeholder="/healthz"
                />
              </Field>
            )}
          </CardContent>
        </Card>
        <div className="flex gap-3">
          <Button type="submit" disabled={save.isPending}>
            {save.isPending
              ? "Saving…"
              : base
                ? "Save changes"
                : "Create app"}
          </Button>
          <Button asChild variant="ghost">
            <Link
              to={
                base ? `/apps/${encodeURIComponent(base.name)}` : "/apps"
              }
            >
              Cancel
            </Link>
          </Button>
        </div>
      </form>
    </section>
  );
}
