import { Link } from "react-router-dom"
import { Button } from "@/components/ui/button"

export default function NotFoundPage() {
  return (
    <div className="grid min-h-screen place-items-center bg-background px-4 text-foreground">
      <div className="space-y-4 text-center">
        <p className="text-sm font-medium text-primary">404</p>
        <h1 className="text-2xl font-semibold tracking-tight">Page not found</h1>
        <p className="text-sm text-muted-foreground">The page you’re looking for doesn’t exist.</p>
        <Button>
          <Link to="/">Back to home</Link>
        </Button>
      </div>
    </div>
  )
}