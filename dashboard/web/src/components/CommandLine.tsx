import { Button } from "@/components/ui/button";

// CommandLine shows a copyable shell command the admin must run outside the
// dashboard (the wizard's rollout prompts, the Settings page's add-node
// recipe). Multi-line commands render intact — whitespace is preserved.
export function CommandLine({ command }: { command: string }) {
  return (
    <div className="flex items-center gap-2">
      <code className="bg-terminal text-foreground flex-1 overflow-x-auto whitespace-pre rounded-lg border px-3 py-2 font-mono text-xs">
        {command}
      </code>
      <Button
        type="button"
        variant="outline"
        size="sm"
        // The command in the accessible name: several Copy buttons can share a
        // screen, and a bare "Copy" tells a screen-reader user nothing.
        aria-label={`Copy ${command}`}
        onClick={() => {
          void navigator.clipboard.writeText(command).catch(() => {
            // Clipboard access can be denied; the command stays selectable.
          });
        }}
      >
        Copy
      </Button>
    </div>
  );
}
