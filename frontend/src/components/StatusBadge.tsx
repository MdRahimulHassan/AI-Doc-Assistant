import { CheckCircle2, Circle, Loader2, XCircle, type LucideIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import type { DocumentStatus, SummaryStatus } from "@/types/api"

type Status = DocumentStatus | SummaryStatus

interface StatusStyle {
  label: string
  variant: "info" | "success" | "danger" | "secondary"
  icon: LucideIcon
  spin?: boolean
}

const STYLES: Record<Status, StatusStyle> = {
  processing: { label: "Processing", variant: "info", icon: Loader2, spin: true },
  ready: { label: "Ready", variant: "success", icon: CheckCircle2 },
  failed: { label: "Failed", variant: "danger", icon: XCircle },
  none: { label: "Not generated", variant: "secondary", icon: Circle },
}

export function StatusBadge({ status }: { status: Status }) {
  const { label, variant, icon: Icon, spin } = STYLES[status]
  return (
    <Badge variant={variant} className="gap-1">
      <Icon className={`size-3 ${spin ? "animate-spin" : ""}`} />
      {label}
    </Badge>
  )
}