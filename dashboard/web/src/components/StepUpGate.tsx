import { StepUpForm } from "@/auth/StepUpForm";
import { Button } from "@/components/ui/button";
import { isStepUpRequired } from "@/lib/errors";
import { useSession } from "@/shell/session";

// StepUpGate is the 403 step_up_required handler (ADR-0003): when a
// destructive mutation is refused for a stale second factor, it swaps in the
// re-auth form; onConfirmed retries the mutation (TanStack retains the failed
// call's variables until reset). An OIDC session's step-up is a full-page IdP
// round-trip, so that branch loses the in-flight action — the user redoes it
// inside the 5-minute freshness window.
export function StepUpGate({
  error,
  onConfirmed,
  onDismiss,
}: {
  error: unknown;
  onConfirmed: () => void;
  onDismiss: () => void;
}) {
  const session = useSession();
  if (!isStepUpRequired(error)) {
    return null;
  }
  return (
    <div className="bg-card flex flex-col gap-3 rounded-lg border p-4">
      <p className="text-sm font-medium">
        This action needs a fresh identity check.
      </p>
      <StepUpForm oidc={session.oidc} onDone={onConfirmed} />
      <Button
        type="button"
        variant="ghost"
        size="sm"
        onClick={onDismiss}
      >
        Cancel
      </Button>
    </div>
  );
}
