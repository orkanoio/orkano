import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, X } from "lucide-react";
import { useEffect, useRef, useState, type FormEvent } from "react";

import { ApiErrorAlert } from "@/components/ApiErrorAlert";
import { StepUpGate } from "@/components/StepUpGate";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  appsKey,
  repoAllowlistKey,
  setupStatusKey,
  updateRepoAllowlist,
  type RepoAllowlistResponse,
} from "@/lib/api";
import { isStepUpRequired } from "@/lib/errors";

const repoRe = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const maxRepoLength = 140;
const maxRepositories = 400;

function rowsFromRepositories(repositories: string[]): string[] {
  return repositories.length > 0 ? repositories : [""];
}

function validateRows(rows: string[]): {
  repositories: string[];
  errors: string[];
} {
  const repositories: string[] = [];
  const errors = rows.map(() => "");
  const seen = new Set<string>();

  rows.forEach((value, index) => {
    const repository = value.trim();
    if (repository === "") {
      return;
    }
    if (repository.length > maxRepoLength || !repoRe.test(repository)) {
      errors[index] =
        "Use the exact owner/repository form, e.g. orkanoio/orkano.";
      return;
    }
    const normalized = repository.toLowerCase();
    if (seen.has(normalized)) {
      errors[index] = "This repository is already listed.";
      return;
    }
    seen.add(normalized);
    repositories.push(repository);
  });

  return { repositories, errors };
}

export function RepoAllowlistEditor({
  repositories,
  resourceVersion,
  onDirtyChange,
}: {
  repositories: string[];
  resourceVersion: string;
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [rows, setRows] = useState(() => rowsFromRepositories(repositories));
  const [version, setVersion] = useState(resourceVersion);
  const [errors, setErrors] = useState<string[]>([]);
  const [dirty, setDirty] = useState(false);
  const lastPropVersion = useRef(resourceVersion);

  useEffect(() => {
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);

  useEffect(() => {
    const propChanged = resourceVersion !== lastPropVersion.current;
    lastPropVersion.current = resourceVersion;
    if (propChanged && !dirty) {
      setRows(rowsFromRepositories(repositories));
      setVersion(resourceVersion);
    }
  }, [dirty, repositories, resourceVersion]);

  const save = useMutation({
    mutationFn: ({
      repositories: nextRepositories,
      resourceVersion: nextResourceVersion,
    }: RepoAllowlistResponse) =>
      updateRepoAllowlist(nextRepositories, nextResourceVersion),
    onSuccess: (response) => {
      setRows(rowsFromRepositories(response.repositories));
      setVersion(response.resourceVersion);
      setErrors([]);
      setDirty(false);
      queryClient.setQueryData<RepoAllowlistResponse>(
        repoAllowlistKey,
        response,
      );
      void queryClient.invalidateQueries({ queryKey: setupStatusKey });
      void queryClient.invalidateQueries({ queryKey: appsKey });
    },
  });

  const updateRow = (index: number, value: string) => {
    setRows((current) =>
      current.map((repository, rowIndex) =>
        rowIndex === index ? value : repository,
      ),
    );
    setErrors((current) =>
      current.map((error, rowIndex) => (rowIndex === index ? "" : error)),
    );
    setDirty(true);
    save.reset();
  };

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const next = validateRows(rows);
    setErrors(next.errors);
    if (next.errors.every((error) => error === "")) {
      save.mutate({
        repositories: next.repositories,
        resourceVersion: version,
      });
    }
  };

  if (isStepUpRequired(save.error) && save.variables) {
    return (
      <StepUpGate
        error={save.error}
        onConfirmed={() => {
          save.mutate(save.variables);
        }}
        onDismiss={() => {
          save.reset();
        }}
      />
    );
  }

  return (
    <form className="flex flex-col gap-3" onSubmit={submit}>
      <div className="flex flex-col gap-1">
        <p className="text-sm text-foreground">Repositories allowed to deploy</p>
        <p className="text-xs leading-relaxed text-muted-foreground">
          Use exact{" "}
          <code className="font-mono text-foreground">owner/repository</code>{" "}
          names. Owner-wide entries and wildcards are intentionally unsupported.
        </p>
      </div>
      <ApiErrorAlert error={save.error} />
      {rows.map((repository, index) => {
        const error = errors[index];
        const errorID = `repo-allowlist-${index.toString()}-error`;
        return (
          <div key={index.toString()} className="flex flex-col gap-1">
            <div className="flex items-center gap-2">
              <Input
                className="min-w-0 flex-1 font-mono"
                aria-label={`Allowed repository ${String(index + 1)}`}
                aria-invalid={error ? true : undefined}
                aria-describedby={error ? errorID : undefined}
                placeholder="owner/repository"
                value={repository}
                disabled={save.isPending}
                onChange={(event) => {
                  updateRow(index, event.target.value);
                }}
              />
              <Button
                type="button"
                variant="ghost"
                size="sm"
                aria-label={`Remove repository ${String(index + 1)}`}
                disabled={rows.length === 1 || save.isPending}
                onClick={() => {
                  setRows((current) =>
                    current.filter((_, rowIndex) => rowIndex !== index),
                  );
                  setErrors((current) =>
                    current.filter((_, rowIndex) => rowIndex !== index),
                  );
                  setDirty(true);
                  save.reset();
                }}
              >
                <X aria-hidden="true" />
              </Button>
            </div>
            {error ? (
              <p id={errorID} className="text-sm text-destructive">
                {error}
              </p>
            ) : null}
          </div>
        );
      })}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          variant="ghost"
          size="sm"
          disabled={save.isPending || rows.length >= maxRepositories}
          onClick={() => {
            setRows((current) => [...current, ""]);
            setErrors((current) => [...current, ""]);
            setDirty(true);
            save.reset();
          }}
        >
          <Plus data-icon="inline-start" aria-hidden="true" />
          Add repository
        </Button>
        <Button type="submit" size="sm" disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Save repositories"}
        </Button>
      </div>
      {save.isSuccess ? (
        <Alert variant="success" aria-live="polite">
          <AlertDescription>
            Allowed repositories updated. The receiver picks up the change
            automatically, usually within about two minutes.
          </AlertDescription>
        </Alert>
      ) : null}
      {rows.length === 1 && rows[0]?.trim() === "" ? (
        <p className="text-xs text-muted-foreground">
          Saving an empty list keeps automatic push deploys deny-all.
        </p>
      ) : null}
    </form>
  );
}
