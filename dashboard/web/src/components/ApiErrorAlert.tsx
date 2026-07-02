import { Alert, AlertDescription } from "@/components/ui/alert";
import { apiErrorMessage, isStepUpRequired } from "@/lib/errors";

// ApiErrorAlert renders a failed query/mutation, except step_up_required —
// that one is handled by StepUpGate (re-auth form), not an error message.
// formatMessage lets a screen with its own error vocabulary (the setup
// wizard) map codes before degrading to the shared copy.
export function ApiErrorAlert({
  error,
  formatMessage = apiErrorMessage,
}: {
  error: unknown;
  formatMessage?: (err: unknown) => string;
}) {
  if (!error || isStepUpRequired(error)) {
    return null;
  }
  return (
    <Alert variant="destructive">
      <AlertDescription>{formatMessage(error)}</AlertDescription>
    </Alert>
  );
}
