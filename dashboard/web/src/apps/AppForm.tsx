import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Field } from "@/components/Field";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  appKey,
  appsKey,
  createApp,
  getApp,
  updateApp,
  type AppResponse,
  type AppSpec,
} from "@/lib/api";
import { Link, navigate } from "@/lib/router";

// Client-side mirrors of the CRD constraints (api/v1alpha1): the apiserver's
// 422 carries only a stable `invalid` code, so mirroring is the only way to
// tell the user WHICH field is wrong.
const nameRe = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;
const repoRe = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const pathRe = /^[A-Za-z0-9_./-]+$/;

interface Fields {
  name: string;
  type: "Web" | "Worker";
  repo: string;
  ref: string;
  subPath: string;
  strategy: "Dockerfile" | "Static";
  dockerfilePath: string;
  staticDir: string;
  port: string;
  replicas: string;
  healthPath: string;
}

function fieldsFromApp(app?: AppResponse): Fields {
  const spec = app?.spec;
  return {
    name: app?.name ?? "",
    type: spec?.type ?? "Web",
    repo: spec?.source.github.repo ?? "",
    ref: spec?.source.github.ref ?? "",
    subPath: spec?.source.subPath ?? "",
    strategy: spec?.build.strategy ?? "Dockerfile",
    dockerfilePath: spec?.build.dockerfile?.path ?? "",
    staticDir: spec?.build.static?.dir ?? "",
    port: spec?.port?.toString() ?? "",
    replicas: spec?.replicas?.toString() ?? "1",
    healthPath: spec?.healthCheck?.path ?? "",
  };
}

function validate(f: Fields, creating: boolean): Record<string, string> {
  const errs: Record<string, string> = {};
  if (creating) {
    // 249 rather than the object-name 253: the env editor derives "<name>-env".
    if (f.name.length > 249 || !nameRe.test(f.name)) {
      errs.name =
        "Use lowercase letters, digits, and hyphens (at most 249 characters).";
    }
  }
  if (f.repo.length > 140 || !repoRe.test(f.repo)) {
    errs.repo = "Use the owner/repository form, e.g. orkanoio/orkano.";
  }
  if (f.ref.length > 250) {
    errs.ref = "At most 250 characters.";
  }
  const pathErr = "Use letters, digits, ., _, / and - (no '..').";
  if (f.subPath !== "" && (f.subPath.length > 512 || !pathRe.test(f.subPath) || f.subPath.includes(".."))) {
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
  } else if (
    f.staticDir === "" ||
    f.staticDir.length > 512 ||
    !pathRe.test(f.staticDir) ||
    f.staticDir.includes("..")
  ) {
    errs.staticDir =
      f.staticDir === "" ? "The directory to serve is required." : pathErr;
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
  const spec: AppSpec = {
    ...(base ?? {}),
    source: {
      github: {
        repo: f.repo.trim(),
        ...(f.ref.trim() !== "" && { ref: f.ref.trim() }),
      },
      ...(f.subPath.trim() !== "" && { subPath: f.subPath.trim() }),
    },
    build:
      f.strategy === "Dockerfile"
        ? {
            strategy: "Dockerfile",
            ...(f.dockerfilePath.trim() !== "" && {
              dockerfile: { path: f.dockerfilePath.trim() },
            }),
          }
        : { strategy: "Static", static: { dir: f.staticDir.trim() } },
    type: f.type,
    replicas: Number(f.replicas),
  };
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
      return <p className="text-muted-foreground text-sm">Loading…</p>;
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
      { ...fields, name: fields.name.trim(), repo: fields.repo.trim() },
      !base,
    );
    setErrors(errs);
    if (Object.keys(errs).length === 0) {
      save.mutate();
    }
  };

  return (
    <section className="flex max-w-xl flex-col gap-4">
      <h1 className="text-xl font-semibold">
        {base ? `Edit ${base.name}` : "New app"}
      </h1>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <ApiErrorAlert error={save.error} />
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
            }}
          >
            <option value="Dockerfile">Dockerfile</option>
            <option value="Static">Static site</option>
          </Select>
        </Field>
        {fields.strategy === "Dockerfile" ? (
          <Field
            id="app-dockerfile"
            label="Dockerfile path"
            error={errors.dockerfilePath}
            hint='Empty means "Dockerfile".'
          >
            <Input
              id="app-dockerfile"
              value={fields.dockerfilePath}
              onChange={(e) => {
                set({ dockerfilePath: e.target.value });
              }}
            />
          </Field>
        ) : (
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
        )}
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
