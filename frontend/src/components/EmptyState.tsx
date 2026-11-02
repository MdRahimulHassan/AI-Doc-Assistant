import type { ReactNode } from "react"
import type { LucideIcon } from "lucide-react"
import { cn } from "@/lib/utils"

interface EmptyStateProps {
  icon: LucideIcon
  title: string
  description: string
  action?: ReactNode
  tone?: "default" | "destructive"
}

export function EmptyState({ icon: Icon, title, description, action, tone = "default" }: EmptyStateProps) {
  return (
    <div className="flex flex-col items-center gap-4 rounded-xl border border-dashed border-border px-6 py-12 text-center">
      <span
        className={cn(
          "grid size-12 place-items-center rounded-full",
          tone === "destructive" ? "bg-destructive/10 text-destructive" : "bg-muted text-muted-foreground",
        )}
      >
        <Icon className="size-6" />
      </span>
      <div className="space-y-1">
        <h3 className="font-medium">{title}</h3>
        <p className="max-w-sm text-sm text-muted-foreground">{description}</p>
      </div>
      {action}
    </div>
  )
}