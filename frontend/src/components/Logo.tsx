import { Sparkles } from "lucide-react"
import { cn } from "@/lib/utils"

export function Logo({ inverted = false, className }: { inverted?: boolean; className?: string }) {
  return (
    <div className={cn("flex items-center gap-2", className)}>
      <span
        className={cn(
          "grid size-8 place-items-center rounded-lg text-primary-foreground",
          inverted ? "bg-primary-foreground/15" : "bg-primary",
        )}
      >
        <Sparkles className="size-4" />
      </span>
      <span className="text-sm font-semibold tracking-tight">Knowledge Assistant</span>
    </div>
  )
}