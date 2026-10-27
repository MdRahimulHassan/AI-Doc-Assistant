import { Button } from "@/components/ui/button"
import { Toaster } from "@/components/ui/sonner"

export default function App() {
  return (
    <div className="grid min-h-screen place-items-center bg-background text-foreground">
      <div className="space-y-4 text-center">
        <h1 className="text-2xl font-semibold tracking-tight">
          AI Knowledge Assistant
        </h1>
        <Button>Setup works</Button>
      </div>
      <Toaster />
    </div>
  )
}