import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ShieldAlert, Upload } from "lucide-react";
import { useState, type FormEvent } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { Field } from "@/components/Field";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  appKey,
  appsKey,
  featuresKey,
  fetchFeatures,
  sourceKind,
  updateAppSource,
  uploadSourceArchive,
  type AppResponse,
  type AppSource,
  type BuildStrategy,
  type FeatureStatus,
  type SourceKind,
} from "@/lib/api";

const repoRe = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const pathRe = /^[A-Za-z0-9_./-]+$/;
const zipNameRe = /^[A-Za-z0-9][A-Za-z0-9._-]{0,250}[.]zip$/;

type UnsafeFeatureID = FeatureStatus["id"];

interface SourceFields {
  kind: SourceKind;
  repo: string;
  gitURL: string;
  ref: string;
  subPath: string;
  uploadDigest: string;
  uploadFileName: string;
  strategy: BuildStrategy["strategy"];
  dockerfilePath: string;
  staticDir: string;
  nixpacksConfigPath: string;
}

function fieldsFromApp(app: AppResponse): SourceFields {
  const source = app.spec.source;
  const kind = sourceKind(source);
  return {
    kind,
    repo: "github" in source ? source.github.repo : "",
    gitURL: "git" in source ? source.git.url : "",
    ref:
      "github" in source
        ? (source.github.ref ?? "")
        : "git" in source
          ? (source.git.ref ?? "")
          : "",
    subPath: source.subPath ?? "",
    uploadDigest: "upload" in source ? source.upload.digest : "",
    uploadFileName:
      "upload" in source ? (source.upload.fileName ?? "") : "",
    strategy: app.spec.build.strategy,
    dockerfilePath: app.spec.build.dockerfile?.path ?? "",
    staticDir: app.spec.build.static?.dir ?? "",
    nixpacksConfigPath: app.spec.build.nixpacks?.configPath ?? "",
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
  fields: SourceFields,
  file: File | null,
  enabled: (id: UnsafeFeatureID) => boolean,
): Record<string, string> {
  const errors: Record<string, string> = {};
  if (fields.kind === "github") {
    if (fields.repo.length > 140 || !repoRe.test(fields.repo)) {
      errors.repo = "Use the owner/repository form, e.g. orkanoio/orkano.";
    }
  } else if (fields.kind === "git") {
    if (!enabled("source.git")) {
      errors.kind = "Generic Git is not enabled for this installation.";
    } else if (fields.gitURL.length > 2048 || !validPublicGitURL(fields.gitURL)) {
      errors.gitURL =
        "Use a public HTTPS repository URL without credentials, a query, or a fragment.";
    }
  } else {
    if (!enabled("source.zip")) {
      errors.kind = "ZIP uploads are not enabled for this installation.";
    } else if (file === null && fields.uploadDigest === "") {
      errors.archive = "Choose a ZIP archive.";
    } else if (file !== null && !zipNameRe.test(file.name)) {
      errors.archive =
        "Use a safe filename with letters, digits, ., _, or - and a lowercase .zip extension.";
    }
  }
  if (
    fields.ref !== "" &&
    (fields.ref.length > 250 ||
      !pathRe.test(fields.ref) ||
      fields.ref.includes(".."))
  ) {
    errors.ref = "Use letters, digits, ., _, / and - (no '..').";
  }
  const pathError = "Use letters, digits, ., _, / and - (no '..').";
  if (
    fields.subPath !== "" &&
    (fields.subPath.length > 512 ||
      !pathRe.test(fields.subPath) ||
      fields.subPath.includes(".."))
  ) {
    errors.subPath = pathError;
  }
  if (fields.strategy === "Dockerfile") {
    if (
      fields.dockerfilePath !== "" &&
      (fields.dockerfilePath.length > 512 ||
        !pathRe.test(fields.dockerfilePath) ||
        fields.dockerfilePath.includes(".."))
    ) {
      errors.dockerfilePath = pathError;
    }
  } else if (fields.strategy === "Static") {
    if (
      fields.staticDir === "" ||
      fields.staticDir.length > 512 ||
      !pathRe.test(fields.staticDir) ||
      fields.staticDir.includes("..")
    ) {
      errors.staticDir =
        fields.staticDir === ""
          ? "The directory to serve is required."
          : pathError;
    }
  } else {
    if (!enabled("build.nixpacks")) {
      errors.strategy = "Nixpacks is not enabled for this installation.";
    }
    if (
      fields.nixpacksConfigPath !== "" &&
      (fields.nixpacksConfigPath.length > 512 ||
        !pathRe.test(fields.nixpacksConfigPath) ||
        fields.nixpacksConfigPath.includes(".."))
    ) {
      errors.nixpacksConfigPath = pathError;
    }
  }
  return errors;
}

function buildStrategy(fields: SourceFields): BuildStrategy {
  if (fields.strategy === "Dockerfile") {
    return {
      strategy: "Dockerfile",
      ...(fields.dockerfilePath.trim() !== "" && {
        dockerfile: { path: fields.dockerfilePath.trim() },
      }),
    };
  }
  if (fields.strategy === "Static") {
    return {
      strategy: "Static",
      static: { dir: fields.staticDir.trim() },
    };
  }
  return {
    strategy: "Nixpacks",
    nixpacks:
      fields.nixpacksConfigPath.trim() === ""
        ? {}
        : { configPath: fields.nixpacksConfigPath.trim() },
  };
}

function installHint(id: UnsafeFeatureID): string {
  return `orkano init --enable-unsafe-feature ${id}`;
}

export function SourceCard({ app }: { app: AppResponse }) {
  const queryClient = useQueryClient();
  const [fields, setFields] = useState<SourceFields>(() => fieldsFromApp(app));
  const [archive, setArchive] = useState<File | null>(null);
  const [archiveInputKey, setArchiveInputKey] = useState(0);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const featureQuery = useQuery({
    queryKey: featuresKey,
    queryFn: fetchFeatures,
    staleTime: 60_000,
  });
  const enabled = (id: UnsafeFeatureID) =>
    featureQuery.data?.some((feature) => feature.id === id && feature.enabled) ??
    false;
  const set = (patch: Partial<SourceFields>) => {
    setFields((current) => ({ ...current, ...patch }));
  };

  const save = useMutation({
    mutationFn: async () => {
      let upload = {
        digest: fields.uploadDigest,
        fileName: fields.uploadFileName,
      };
      if (fields.kind === "upload" && archive !== null) {
        upload = await uploadSourceArchive(app.name, archive);
      }
      const common =
        fields.subPath.trim() === "" ? {} : { subPath: fields.subPath.trim() };
      let source: AppSource;
      if (fields.kind === "github") {
        source = {
          github: {
            repo: fields.repo.trim(),
            ...(fields.ref.trim() !== "" && { ref: fields.ref.trim() }),
          },
          ...common,
        };
      } else if (fields.kind === "git") {
        source = {
          git: {
            url: fields.gitURL.trim(),
            ...(fields.ref.trim() !== "" && { ref: fields.ref.trim() }),
          },
          ...common,
        };
      } else {
        source = { upload, ...common };
      }
      return updateAppSource(app.name, source, buildStrategy(fields));
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(appKey(app.name), updated);
      void queryClient.invalidateQueries({ queryKey: appsKey });
      setArchive(null);
      setArchiveInputKey((current) => current + 1);
      setFields(fieldsFromApp(updated));
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    save.reset();
    const nextErrors = validate(
      {
        ...fields,
        repo: fields.repo.trim(),
        gitURL: fields.gitURL.trim(),
        ref: fields.ref.trim(),
        subPath: fields.subPath.trim(),
        dockerfilePath: fields.dockerfilePath.trim(),
        staticDir: fields.staticDir.trim(),
        nixpacksConfigPath: fields.nixpacksConfigPath.trim(),
      },
      archive,
      enabled,
    );
    setErrors(nextErrors);
    if (Object.keys(nextErrors).length === 0) {
      save.mutate();
    }
  };

  const selectedFeatures = featureQuery.data?.filter(
    (feature) =>
      (fields.kind === "git" && feature.id === "source.git") ||
      (fields.kind === "upload" && feature.id === "source.zip") ||
      (fields.strategy === "Nixpacks" && feature.id === "build.nixpacks"),
  );

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-center gap-2">
          <CardTitle role="heading" aria-level={2}>
            Source
          </CardTitle>
          <Badge variant="secondary">build input</Badge>
        </div>
        <CardDescription>
          Choose where the code comes from and how Orkano turns it into an
          image. Saving starts a new deployment when the source changes.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form className="flex max-w-2xl flex-col gap-5" onSubmit={submit}>
          <ApiErrorAlert error={featureQuery.error} />
          <ApiErrorAlert error={save.error} />
          {save.isSuccess ? (
            <Alert variant="success">
              <AlertTitle>Source saved</AlertTitle>
              <AlertDescription>
                The next build will use this source and build method.
              </AlertDescription>
            </Alert>
          ) : null}

          <Field
            id="source-kind"
            label="Source provider"
            error={errors.kind}
            hint="GitHub App is the secure default. Generic Git and ZIP uploads must be enabled during installation."
          >
            <Select
              id="source-kind"
              value={fields.kind}
              onChange={(event) => {
                set({ kind: event.target.value as SourceKind });
                setArchive(null);
                setArchiveInputKey((current) => current + 1);
                setErrors({});
                save.reset();
              }}
            >
              <option value="github">GitHub App</option>
              <option value="git" disabled={!enabled("source.git")}>
                Generic Git — unsafe{enabled("source.git") ? "" : " (disabled)"}
              </option>
              <option value="upload" disabled={!enabled("source.zip")}>
                Upload ZIP — unsafe{enabled("source.zip") ? "" : " (disabled)"}
              </option>
            </Select>
          </Field>

          {fields.kind === "github" ? (
            <Field
              id="source-repo"
              label="GitHub repository"
              error={errors.repo}
              hint="owner/repository"
            >
              <Input
                id="source-repo"
                value={fields.repo}
                onChange={(event) => {
                  set({ repo: event.target.value });
                }}
                placeholder="orkanoio/example"
                required
              />
            </Field>
          ) : null}

          {fields.kind === "git" ? (
            <Field
              id="source-git-url"
              label="Public Git URL"
              error={errors.gitURL}
              hint="HTTPS only. Credentials, query parameters, and URL fragments are rejected."
            >
              <Input
                id="source-git-url"
                type="url"
                value={fields.gitURL}
                onChange={(event) => {
                  set({ gitURL: event.target.value });
                }}
                placeholder="https://git.example.com/team/app.git"
                required
              />
            </Field>
          ) : null}

          {fields.kind === "upload" ? (
            <Field
              id="source-archive"
              label="ZIP archive"
              error={errors.archive}
              hint={
                fields.uploadDigest === ""
                  ? "The archive is uploaded and addressed by its SHA-256 digest."
                  : `Current: ${fields.uploadFileName || fields.uploadDigest}`
              }
            >
              <Input
                key={archiveInputKey}
                id="source-archive"
                type="file"
                accept=".zip,application/zip"
                onChange={(event) => {
                  setArchive(event.target.files?.[0] ?? null);
                }}
              />
            </Field>
          ) : null}

          {fields.kind !== "upload" ? (
            <Field
              id="source-ref"
              label="Branch or tag"
              error={errors.ref}
              hint="Empty means the repository's default branch."
            >
              <Input
                id="source-ref"
                value={fields.ref}
                onChange={(event) => {
                  set({ ref: event.target.value });
                }}
              />
            </Field>
          ) : null}

          <Field
            id="source-subpath"
            label="Subdirectory"
            error={errors.subPath}
            hint="Build from this directory inside the repository or archive."
          >
            <Input
              id="source-subpath"
              value={fields.subPath}
              onChange={(event) => {
                set({ subPath: event.target.value });
              }}
            />
          </Field>

          <div className="border-t pt-5">
            <Field
              id="source-strategy"
              label="Build method"
              error={errors.strategy}
              hint="Dockerfile and Static are built in Orkano's confined BuildKit job."
            >
              <Select
                id="source-strategy"
                value={fields.strategy}
                onChange={(event) => {
                  set({
                    strategy: event.target.value as BuildStrategy["strategy"],
                  });
                  setErrors({});
                  save.reset();
                }}
              >
                <option value="Dockerfile">Dockerfile</option>
                <option value="Static">Static site</option>
                <option
                  value="Nixpacks"
                  disabled={!enabled("build.nixpacks")}
                >
                  Nixpacks — unsafe
                  {enabled("build.nixpacks") ? "" : " (disabled)"}
                </option>
              </Select>
            </Field>
          </div>

          {fields.strategy === "Dockerfile" ? (
            <Field
              id="source-dockerfile"
              label="Dockerfile path"
              error={errors.dockerfilePath}
              hint={'Empty means "Dockerfile".'}
            >
              <Input
                id="source-dockerfile"
                value={fields.dockerfilePath}
                onChange={(event) => {
                  set({ dockerfilePath: event.target.value });
                }}
              />
            </Field>
          ) : null}

          {fields.strategy === "Static" ? (
            <Field
              id="source-static-dir"
              label="Directory to serve"
              error={errors.staticDir}
              hint="The built output directory, e.g. public or dist."
            >
              <Input
                id="source-static-dir"
                value={fields.staticDir}
                onChange={(event) => {
                  set({ staticDir: event.target.value });
                }}
                required
              />
            </Field>
          ) : null}

          {fields.strategy === "Nixpacks" ? (
            <Field
              id="source-nixpacks-config"
              label="Nixpacks config path"
              error={errors.nixpacksConfigPath}
              hint={'Optional path to a Nixpacks TOML file, e.g. "nixpacks.toml".'}
            >
              <Input
                id="source-nixpacks-config"
                value={fields.nixpacksConfigPath}
                onChange={(event) => {
                  set({ nixpacksConfigPath: event.target.value });
                }}
              />
            </Field>
          ) : null}

          {selectedFeatures?.map((feature) => (
            <UnsafeFeatureNotice key={feature.id} feature={feature} />
          ))}

          <Button type="submit" className="self-start" disabled={save.isPending}>
            {save.isPending ? (
              <>
                {fields.kind === "upload" && archive !== null ? (
                  <Upload aria-hidden="true" />
                ) : null}
                Saving…
              </>
            ) : (
              "Save source"
            )}
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

export function UnsafeFeatureNotice({ feature }: { feature: FeatureStatus }) {
  return (
    <Alert className="border-warning/30 bg-warning/8 text-warning">
      <ShieldAlert aria-hidden="true" />
      <AlertTitle>
        Unsafe feature
        <Badge variant="warning" className="ml-2 align-middle">
          {feature.enabled ? "enabled" : "disabled"}
        </Badge>
      </AlertTitle>
      <AlertDescription className="text-warning/90">
        <p>{feature.description}</p>
        {!feature.enabled ? (
          <p>
            Enable it during installation with{" "}
            <code className="break-all font-mono text-[12px] text-foreground">
              {installHint(feature.id)}
            </code>
            .
          </p>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}
