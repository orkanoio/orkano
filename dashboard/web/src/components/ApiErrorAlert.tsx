import { Alert, AlertDescription } from "@/components/ui/alert";
import { apiErrorMessage, isStepUpRequired } from "@/lib/errors";

// ApiErrorAlert renders a failed query/mutation, except step_up_required —
// that one is handled by StepUpGate (re-auth form), not an error message.
export function ApiErrorAlert({ error }: { error: unknown }) {
  if (!error || isStepUpRequired(error)) {
    return null;
  }
  return (
    <Alert variant="destructive">
      <AlertDescription>{apiErrorMessage(error)}</AlertDescription>
    </Alert>
  );
}
