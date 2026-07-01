import { useMutation } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { oidcStepUpPath, stepUp } from "@/lib/api";

import { authErrorMessage } from "./messages";

// StepUpForm is the re-authentication gate for destructive actions
// (ADR-0003). The branch is by session type: an OIDC session re-authenticates
// at the IdP with a full-page round-trip (the password/TOTP endpoint refuses
// it with oidc_stepup_required), while a local admin re-proves password +
// authenticator code in place. The App/catalog screens (M2.6 sub-commit 5)
// open this when a destructive call fails with 403 step_up_required.
export function StepUpForm({
  oidc,
  onDone,
}: {
  oidc: boolean;
  onDone?: () => void;
}) {
  if (oidc) {
    return (
      <div className="flex flex-col gap-3">
        <p className="text-muted-foreground text-sm">
          This session signs in through your identity provider — re-authenticate
          there to continue.
        </p>
        <Button asChild className="w-full">
          <a href={oidcStepUpPath}>Re-authenticate with SSO</a>
        </Button>
      </div>
    );
  }
  return <LocalStepUpForm onDone={onDone} />;
}

function LocalStepUpForm({ onDone }: { onDone?: () => void }) {
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");

  const confirm = useMutation({
    mutationFn: () => stepUp(password, code.trim()),
    onSuccess: () => onDone?.(),
  });

  const error = confirm.isError ? authErrorMessage(confirm.error) : null;

  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        confirm.mutate();
      }}
    >
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <p className="text-muted-foreground text-sm">
        Confirm your password and a fresh authenticator code to continue.
      </p>
      <div className="flex flex-col gap-2">
        <Label htmlFor="stepup-password">Password</Label>
        <Input
          id="stepup-password"
          type="password"
          value={password}
          onChange={(e) => {
            setPassword(e.target.value);
          }}
          autoComplete="current-password"
          autoFocus
          required
        />
      </div>
      <div className="flex flex-col gap-2">
        <Label htmlFor="stepup-code">Authenticator code</Label>
        <Input
          id="stepup-code"
          inputMode="numeric"
          autoComplete="one-time-code"
          maxLength={6}
          value={code}
          onChange={(e) => {
            setCode(e.target.value);
          }}
          required
        />
      </div>
      <Button type="submit" className="w-full" disabled={confirm.isPending}>
        {confirm.isPending ? "Confirming…" : "Confirm identity"}
      </Button>
    </form>
  );
}
