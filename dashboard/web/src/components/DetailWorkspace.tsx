import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";

export interface DetailSection<SectionID extends string> {
  id: SectionID;
  label: string;
  icon: LucideIcon;
  destructive?: boolean;
}

export function DetailWorkspace<SectionID extends string>({
  sections,
  active,
  onSelect,
  children,
}: {
  sections: readonly DetailSection<SectionID>[];
  active: SectionID;
  onSelect: (section: SectionID) => void;
  children: ReactNode;
}) {
  return (
    <div className="grid min-w-0 gap-4 md:grid-cols-[11rem_minmax(0,1fr)]">
      <Card className="max-w-full min-w-0 self-start overflow-hidden py-2">
        <CardContent className="flex gap-1 overflow-x-auto px-2 md:flex-col">
          {sections.map((section) => {
            const Icon = section.icon;
            const selected = section.id === active;
            return (
              <Button
                key={section.id}
                type="button"
                variant="ghost"
                aria-current={selected ? "page" : undefined}
                className={cn(
                  "justify-start",
                  selected && "bg-primary/10 text-primary",
                  section.destructive && !selected && "text-destructive",
                )}
                onClick={() => {
                  onSelect(section.id);
                }}
              >
                <Icon data-icon="inline-start" aria-hidden="true" />
                {section.label}
              </Button>
            );
          })}
        </CardContent>
      </Card>
      <div className="min-w-0">{children}</div>
    </div>
  );
}
