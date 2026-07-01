import { useMutation } from "@tanstack/react-query";
import { QRCodeSVG } from "qrcode.react";
import { useState, type FormEvent } from "react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  confirmTotp,
  redeemInstallToken,
  type RedeemResponse,
} from "@/lib/api";

import { authErrorMessage, isExpiredChallenge } from "./messages";
import { SSOSignIn } from "./sso";

// Byte-length mirror of dashboard/internal/auth ValidatePasswordPolicy —
// bytes, not code points, because bcrypt silently ignores bytes past 72. The
// two messages deliberately use different units: "characters" is accurate for
// the minimum whenever it fires (a character is at least one byte, so under
// 12 bytes there are under 12 characters), while the maximum must say "bytes"
// — 72 characters of multi-byte text can exceed it.
const minPasswordBytes = 12;
const maxPasswordBytes = 72;

function passwordPolicyError(password: string): string | null {
  const bytes = new TextEncoder().encode(password).length;
  if (bytes < minPasswordBytes) {
    return `Passwords must be at least ${minPasswordBytes.toString()} characters.`;
  }
  if (bytes > maxPasswordBytes) {
    return `Passwords must be at most ${maxPasswordBytes.toString()} bytes.`;
  }
  return null;
}

type Step =
  | { id: "redeem"; notice?: string }
  | { id: "recovery"; enrollment: RedeemResponse }
  | { id: "enroll"; otpauthUrl: string };

// BootstrapFlow walks the one-time install-token redemption (ADR-0003):
// redeem creates the admin, the recovery codes are saved (shown exactly
// once), and the forced TOTP enrollment confirms a live authenticator code
// before the server mints the first session. State lives in memory only —
// abandoning mid-flow is safe, a fresh redeem sweeps the unconfirmed admin.
export function BootstrapFlow({
  oidcEnabled,
  onAuthenticated,
}: {
  oidcEnabled: boolean;
  onAuthenticated: (username: string) => void;
}) {
  const [step, setStep] = useState<Step>({ id: "redeem" });

  switch (step.id) {
    case "redeem":
      return (
        <RedeemForm
          notice={step.notice}
          oidcEnabled={oidcEnabled}
          onRedeemed={(enrollment) => {
            setStep({ id: "recovery", enrollment });
          }}
        />
      );
    case "recovery":
      return (
        <RecoveryCodes
          codes={step.enrollment.recoveryCodes}
          onSaved={() => {
            setStep({ id: "enroll", otpauthUrl: step.enrollment.otpauthUrl });
          }}
        />
      );
    case "enroll":
      return (
        <EnrollTOTP
          otpauthUrl={step.otpauthUrl}
          onAuthenticated={onAuthenticated}
          onExpired={() => {
            setStep({
              id: "redeem",
              notice:
                "The enrollment window expired — redeem the install token again.",
            });
          }}
        />
      );
  }
}

function RedeemForm({
  notice,
  oidcEnabled,
  onRedeemed,
}: {
  notice?: string;
  oidcEnabled: boolean;
  onRedeemed: (enrollment: RedeemResponse) => void;
}) {
  const [token, setToken] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [clientError, setClientError] = useState<string | null>(null);

  const redeem = useMutation({
    mutationFn: redeemInstallToken,
    onSuccess: onRedeemed,
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const policyErr = passwordPolicyError(password);
    if (policyErr) {
      setClientError(policyErr);
      return;
    }
    if (password !== confirm) {
      setClientError("Passwords do not match.");
      return;
    }
    setClientError(null);
    redeem.mutate({ token: token.trim(), username: username.trim(), password });
  };

  const error =
    clientError ?? (redeem.isError ? authErrorMessage(redeem.error) : null);

  return (
    <form className="flex flex-col gap-4" onSubmit={submit}>
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
        <Label htmlFor="install-token">Install token</Label>
        {/* Masked: the token is the sole credential that claims the admin
            account — it must not sit readable on a shared screen. */}
        <Input
          id="install-token"
          type="password"
          value={token}
          onChange={(e) => {
            setToken(e.target.value);
          }}
          autoComplete="off"
          required
        />
        <p className="text-muted-foreground text-xs">
          Printed exactly once by <span className="font-mono">orkano init</span>.
        </p>
      </div>
      <div className="flex flex-col gap-2">
        <Label htmlFor="bootstrap-username">Username</Label>
        <Input
          id="bootstrap-username"
          value={username}
          onChange={(e) => {
            setUsername(e.target.value);
          }}
          autoComplete="username"
          required
        />
      </div>
      <div className="flex flex-col gap-2">
        <Label htmlFor="bootstrap-password">Password</Label>
        <Input
          id="bootstrap-password"
          type="password"
          value={password}
          onChange={(e) => {
            setPassword(e.target.value);
          }}
          autoComplete="new-password"
          required
        />
        <p className="text-muted-foreground text-xs">
          At least 12 characters (at most 72 bytes).
        </p>
      </div>
      <div className="flex flex-col gap-2">
        <Label htmlFor="bootstrap-confirm">Confirm password</Label>
        <Input
          id="bootstrap-confirm"
          type="password"
          value={confirm}
          onChange={(e) => {
            setConfirm(e.target.value);
          }}
          autoComplete="new-password"
          required
        />
      </div>
      <Button type="submit" className="w-full" disabled={redeem.isPending}>
        {redeem.isPending ? "Creating admin…" : "Create admin account"}
      </Button>
      <SSOSignIn enabled={oidcEnabled} />
    </form>
  );
}

function RecoveryCodes({
  codes,
  onSaved,
}: {
  codes: string[];
  onSaved: () => void;
}) {
  const [copied, setCopied] = useState(false);
  const canCopy = typeof navigator !== "undefined" && !!navigator.clipboard;

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(codes.join("\n"));
      setCopied(true);
    } catch {
      // Clipboard refused (permissions) — the codes stay visible to copy by hand.
    }
  };

  return (
    <div className="flex flex-col gap-4">
      <p className="text-muted-foreground text-sm">
        Each recovery code signs you in once if you lose the authenticator.
        Store them somewhere safe — they are shown only now.
      </p>
      <ul className="bg-muted/50 grid grid-cols-2 gap-x-6 gap-y-1 rounded-md border p-4 font-mono text-sm">
        {codes.map((code) => (
          <li key={code}>{code}</li>
        ))}
      </ul>
      {canCopy && (
        <Button
          type="button"
          variant="outline"
          className="w-full"
          onClick={() => void copy()}
        >
          {copied ? "Copied" : "Copy codes"}
        </Button>
      )}
      <Button type="button" className="w-full" autoFocus onClick={onSaved}>
        I&apos;ve saved my recovery codes
      </Button>
    </div>
  );
}

// totpSecret extracts the base32 secret from the otpauth:// URL for manual
// entry when the QR code can't be scanned.
function totpSecret(otpauthUrl: string): string | null {
  try {
    return new URL(otpauthUrl).searchParams.get("secret");
  } catch {
    return null;
  }
}

function EnrollTOTP({
  otpauthUrl,
  onAuthenticated,
  onExpired,
}: {
  otpauthUrl: string;
  onAuthenticated: (username: string) => void;
  onExpired: () => void;
}) {
  const [code, setCode] = useState("");

  const confirm = useMutation({
    mutationFn: confirmTotp,
    onSuccess: (result) => {
      onAuthenticated(result.username);
    },
    onError: (err) => {
      if (isExpiredChallenge(err)) {
        onExpired();
      }
    },
  });

  const secret = totpSecret(otpauthUrl);
  const error =
    confirm.isError && !isExpiredChallenge(confirm.error)
      ? authErrorMessage(confirm.error)
      : null;

  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        confirm.mutate(code.trim());
      }}
    >
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <p className="text-muted-foreground text-sm">
        Scan the QR code with an authenticator app — two-factor sign-in is
        required — then enter the 6-digit code it shows.
      </p>
      {/* White padding box: QR readers need a quiet zone and a light background
          regardless of theme. */}
      <div className="self-center rounded-md border bg-white p-3">
        <QRCodeSVG value={otpauthUrl} size={176} />
      </div>
      {secret && (
        <p className="text-muted-foreground text-xs">
          Can&apos;t scan? Enter this secret manually:{" "}
          <span className="font-mono break-all select-all">{secret}</span>
        </p>
      )}
      <div className="flex flex-col gap-2">
        <Label htmlFor="enroll-code">Authenticator code</Label>
        <Input
          id="enroll-code"
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
      <Button type="submit" className="w-full" disabled={confirm.isPending}>
        {confirm.isPending ? "Verifying…" : "Verify and finish setup"}
      </Button>
    </form>
  );
}
