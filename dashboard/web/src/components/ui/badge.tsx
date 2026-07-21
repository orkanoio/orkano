import { cva, type VariantProps } from "class-variance-authority";
import * as React from "react";

import { cn } from "@/lib/utils";

// Vendored shadcn/ui badge, restyled onto the landing design system: mono
// pill chips with a tinted fill + border (the landing's "healthy" /
// "type: database" chips). success/warning join the stock variants so status
// UI can speak the landing's green/amber/red without raw hex.

const badgeVariants = cva(
  "inline-flex items-center justify-center gap-1.5 rounded-full border px-2.5 py-0.5 font-mono text-[11px] leading-4 w-fit whitespace-nowrap shrink-0 [&>svg]:size-3 [&>svg]:pointer-events-none overflow-hidden",
  {
    variants: {
      variant: {
        default: "border-primary/25 bg-primary/8 text-primary",
        success: "border-success/25 bg-success/8 text-success",
        warning: "border-warning/25 bg-warning/8 text-warning",
        destructive:
          "border-destructive/25 bg-destructive/8 text-destructive",
        secondary: "border-white/15 bg-white/4 text-muted-foreground",
        outline: "border-white/15 text-foreground",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  },
);

function Badge({
  className,
  variant,
  ...props
}: React.ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return (
    <span
      data-slot="badge"
      className={cn(badgeVariants({ variant }), className)}
      {...props}
    />
  );
}

export { Badge, badgeVariants };
