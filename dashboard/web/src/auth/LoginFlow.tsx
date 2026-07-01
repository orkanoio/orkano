import { useMutation } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { login, loginTotp } from "@/lib/api";

import { authErrorMessage, isExpiredChallenge } from "./messages";
import { SSOSignIn } from "./sso";

type Step = { id: "credentials"; notice?: string } | { id: "totp" };

// LoginFlow is the local admin's two-step sign-in (ADR-0003): password first,
// then a live authenticator code or a single-use recovery code. The server
// tracks the step with a short-lived challenge cookie; when it expires the
// flow drops back to the first step with a notice.
export function LoginFlow({
  oidcEnabled,
  onAuthenticated,
}: {
  oidcEnabled: boolean;
  onAuthenticated: (username: string) => void;
}) {
  const [step, setStep] = useState<Step>({ id: "credentials" });

  if (step.id === "credentials") {
    return (
      <CredentialsForm
        notice={step.notice}
        oidcEnabled={oidcEnabled}
        onFirstFactor={() => {
          setStep({ id: "totp" });
        }}
      />
    );
  }
  return (
    <SecondFactorForm
      onAuthenticated={onAuthenticated}
      onExpired={() => {
        setStep({
          id: "credentials",
          notice: "That sign-in attempt expired — start again.",
        });
      }}
    />
  );
}

function CredentialsForm({
  notice,
  oidcEnabled,
  onFirstFactor,
}: {
  notice?: string;
  oidcEnabled: boolean;
  onFirstFactor: () => void;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  const signIn = useMutation({
    mutationFn: () => login(username.trim(), password),
    onSuccess: onFirstFactor,
  });

  const error = signIn.isError ? authErrorMessage(signIn.error) : null;

  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        signIn.mutate();
      }}
    >
      {notice && (
        <Alert>
          <AlertDescription>{notice}</AlertDescription>
        </Alert>
      )}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="flex flex-col gap-2">
        <Label htmlFor="login-username">Username</Label>
        <Input
          id="login-username"
          value={username}
          onChange={(e) => {
            setUsername(e.target.value);
          }}
          autoComplete="username"
          required
        />
      </div>
      <div className="flex flex-col gap-2">
        <Label htmlFor="login-password">Password</Label>
        <Input
          id="login-password"
          type="password"
          value={password}
          onChange={(e) => {
            setPassword(e.target.value);
          }}
          autoComplete="current-password"
          required
        />
      </div>
      <Button type="submit" className="w-full" disabled={signIn.isPending}>
        {signIn.isPending ? "Signing in…" : "Sign in"}
      </Button>
      <SSOSignIn enabled={oidcEnabled} />
    </form>
  );
}

function SecondFactorForm({
  onAuthenticated,
  onExpired,
}: {
  onAuthenticated: (username: string) => void;
  onExpired: () => void;
}) {
  const [mode, setMode] = useState<"totp" | "recovery">("totp");
  const [code, setCode] = useState("");
  const [recoveryCode, setRecoveryCode] = useState("");

  const verify = useMutation({
    mutationFn: () =>
      mode === "totp"
        ? loginTotp({ code: code.trim() })
        : loginTotp({ recoveryCode: recoveryCode.trim() }),
    onSuccess: (result) => {
      onAuthenticated(result.username);
    },
    onError: (err) => {
      if (isExpiredChallenge(err)) {
        onExpired();
      }
    },
  });

  const error =
    verify.isError && !isExpiredChallenge(verify.error)
      ? authErrorMessage(verify.error)
      : null;

  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        verify.mutate();
      }}
    >
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {mode === "totp" ? (
        <div className="flex flex-col gap-2">
          <Label htmlFor="login-code">Authenticator code</Label>
          <Input
            id="login-code"
            inputMode="numeric"
            autoComplete="one-time-code"
            maxLength={6}
            value={code}
            onChange={(e) => {
              setCode(e.target.value);
            }}
            autoFocus
            required
          />
        </div>
      ) : (
        <div className="flex flex-col gap-2">
          <Label htmlFor="login-recovery">Recovery code</Label>
          <Input
            id="login-recovery"
            autoComplete="off"
            value={recoveryCode}
            onChange={(e) => {
              setRecoveryCode(e.target.value);
            }}
            autoFocus
            required
          />
          <p className="text-muted-foreground text-xs">
            Recovery codes work once each.
          </p>
        </div>
      )}
      <Button type="submit" className="w-full" disabled={verify.isPending}>
        {verify.isPending ? "Verifying…" : "Verify"}
      </Button>
      <Button
        type="button"
        variant="link"
        size="sm"
        className="self-start px-0"
        onClick={() => {
          // A mode switch is a fresh attempt: drop the previous try's error
          // and both typed values so nothing stale carries across.
          verify.reset();
          setCode("");
          setRecoveryCode("");
          setMode(mode === "totp" ? "recovery" : "totp");
        }}
      >
        {mode === "totp"
          ? "Use a recovery code instead"
          : "Use the authenticator code instead"}
      </Button>
    </form>
  );
}
