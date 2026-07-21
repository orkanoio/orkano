import * as React from "react";

import { cn } from "@/lib/utils";

// Vendored shadcn/ui input, restyled onto the landing design system: a dark
// inset well (page background against the card) with mono type — form values
// here are repos, hosts, tokens, sizes.

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "file:text-foreground placeholder:text-muted-foreground border-input bg-background flex h-9 w-full min-w-0 rounded-md border px-3 py-1 font-mono text-[13px] transition-[color,box-shadow] outline-none file:inline-flex file:h-7 file:border-0 file:bg-transparent file:text-sm file:font-medium disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50",
        "focus-visible:border-ring focus-visible:ring-ring/50 focus-visible:ring-[3px]",
        "aria-invalid:ring-destructive/20 aria-invalid:border-destructive",
        className,
      )}
      {...props}
    />
  );
}

export { Input };
