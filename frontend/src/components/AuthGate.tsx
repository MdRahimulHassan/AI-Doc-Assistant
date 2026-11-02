import type { ReactNode } from "react"
import { WifiOff } from "lucide-react"
import { Button } from "@/components/ui/button"
import { FullPageLoader } from "@/components/FullPageLoader"
import { useAuth } from "@/hooks/useAuth"

function BootError() {
  const { retry, logout } = useAuth()
  return (
    <div className="grid min-h-screen place-items-center bg-background px-4 text-foreground">
      <div className="max-w-sm space-y-6 text-center">
        <span className="mx-auto grid size-12 place-items-center rounded-full bg-muted text-muted-foreground">
          <WifiOff className="size-5" />
        </span>
        <div className="space-y-2">
          <h1 className="text-lg font-semibold">Can’t reach the server</h1>
          <p className="text-sm text-muted-foreground">
            We couldn’t verify your session. Check your connection and try again.
          </p>
        </div>
        <div className="flex justify-center gap-3">
          <Button onClick={retry}>Try again</Button>
          <Button variant="ghost" onClick={logout}>
            Sign in again
          </Button>
        </div>
      </div>
    </div>
  )
}

export function AuthGate({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  if (status === "loading") return <FullPageLoader />
  if (status === "error") return <BootError />
  return <>{children}</>
}