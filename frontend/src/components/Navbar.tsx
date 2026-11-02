import { Link, useNavigate } from "react-router-dom"
import { LogOut } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Logo } from "@/components/Logo"
import { ThemeToggle } from "@/components/ThemeToggle"
import { useAuth } from "@/hooks/useAuth"

export function Navbar() {
  const { user, logout } = useAuth()
  const navigate = useNavigate()
  if (!user) return null // unreachable inside ProtectedRoute; narrows the type

  const initial = user.email.charAt(0).toUpperCase()

  function handleSignOut() {
    logout()
    // Explicit navigation (no "from"): after a deliberate sign-out, the next login starts at the dashboard.
    navigate("/login", { replace: true })
  }

  return (
    <header className="sticky top-0 z-40 border-b border-border bg-background/80 backdrop-blur">
      <div className="mx-auto flex h-14 max-w-6xl items-center justify-between px-4 sm:px-6">
        <Link to="/" aria-label="Home">
          <Logo />
        </Link>

        <div className="flex items-center gap-2">
          <ThemeToggle />
          <DropdownMenu>
            <DropdownMenuTrigger>
              <Button variant="ghost" size="icon" className="rounded-full" aria-label="Account menu">
                <span className="grid size-7 place-items-center rounded-full bg-primary text-xs font-medium text-primary-foreground">
                  {initial}
                </span>
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-56">
              <DropdownMenuLabel className="space-y-1 font-normal">
                <p className="text-xs text-muted-foreground">Signed in as</p>
                <p className="truncate text-sm font-medium">{user.email}</p>
              </DropdownMenuLabel>
              <DropdownMenuSeparator />
              <DropdownMenuItem onClick={handleSignOut}>
                <LogOut /> Sign out
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
    </header>
  )
}