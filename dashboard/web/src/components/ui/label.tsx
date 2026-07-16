import * as React from "react";

import { cn } from "@/lib/utils";

// Vendored shadcn/ui label, on a plain <label> instead of
// @radix-ui/react-label — the Radix wrapper only adds double-click
// text-selection suppression, not worth a dependency. Styled as the landing's
// mono uppercase field label.

function Label({ className, ...props }: React.ComponentProps<"label">) {
  return (
    <label
      data-slot="label"
      className={cn(
        "text-muted-foreground flex items-center gap-2 font-mono text-[11px] leading-none tracking-[0.08em] uppercase select-none group-data-[disabled=true]:pointer-events-none group-data-[disabled=true]:opacity-50 peer-disabled:cursor-not-allowed peer-disabled:opacity-50",
        className,
      )}
      {...props}
    />
  );
}

export { Label };
