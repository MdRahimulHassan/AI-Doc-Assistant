import { Loader2 } from "lucide-react"
import { Logo } from "@/components/Logo"

export function FullPageLoader() {
  return (
    <div
      role="status"
      aria-live="polite"
      className="grid min-h-screen place-items-center bg-background text-foreground"
    >
      <div className="flex flex-col items-center gap-4">
        <Logo />
        <Loader2 className="size-5 animate-spin text-muted-foreground" />
        <span className="sr-only">Loading…</span>
      </div>
    </div>
  )
}