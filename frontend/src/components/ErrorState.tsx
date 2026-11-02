import { AlertTriangle, Loader2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { EmptyState } from "@/components/EmptyState"

interface ErrorStateProps {
  title?: string
  message: string
  onRetry: () => void
  retrying?: boolean
}

export function ErrorState({ title = "Something went wrong", message, onRetry, retrying = false }: ErrorStateProps) {
  return (
    <EmptyState
      icon={AlertTriangle}
      tone="destructive"
      title={title}
      description={message}
      action={
        <Button variant="outline" onClick={onRetry} disabled={retrying}>
          {retrying && <Loader2 className="animate-spin" />}
          Try again
        </Button>
      }
    />
  )
}