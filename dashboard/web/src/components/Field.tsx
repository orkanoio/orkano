import { cloneElement, type ReactElement } from "react";

import { Label } from "@/components/ui/label";

// Field is the labeled-input layout the forms share. It wires the validation
// error (or the hint) to the input via aria-describedby/aria-invalid so
// assistive tech announces WHICH field failed — the visual styling alone
// (input.tsx's aria-invalid ring) needs the attribute set anyway.
export function Field({
  id,
  label,
  error,
  hint,
  children,
}: {
  id: string;
  label: string;
  error?: string;
  hint?: string;
  children: ReactElement<{
    "aria-invalid"?: boolean;
    "aria-describedby"?: string;
  }>;
}) {
  return (
    <div className="flex flex-col gap-2">
      <Label htmlFor={id}>{label}</Label>
      {cloneElement(children, {
        "aria-invalid": error ? true : undefined,
        "aria-describedby": error
          ? `${id}-error`
          : hint
            ? `${id}-hint`
            : undefined,
      })}
      {error ? (
        <p id={`${id}-error`} className="text-destructive text-xs">
          {error}
        </p>
      ) : (
        hint && (
          <p id={`${id}-hint`} className="text-muted-foreground text-xs">
            {hint}
          </p>
        )
      )}
    </div>
  );
}
