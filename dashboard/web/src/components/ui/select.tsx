import * as React from "react";

import { cn } from "@/lib/utils";

// A styled NATIVE <select> matching the Input styling — deliberately not the
// shadcn Select, which would pull in radix-select for nothing the dashboard's
// few enum fields need (the label/no-radix precedent).

function Select({ className, ...props }: React.ComponentProps<"select">) {
  return (
    <select
      data-slot="select"
      className={cn(
        "border-input bg-background flex h-9 w-full min-w-0 rounded-md border px-3 py-1 font-mono text-[13px] transition-[color,box-shadow] outline-none disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50",
        "focus-visible:border-ring focus-visible:ring-ring/50 focus-visible:ring-[3px]",
        className,
      )}
      {...props}
    />
  );
}

export { Select };
