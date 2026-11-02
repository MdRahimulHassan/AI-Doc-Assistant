import { Navigate, Outlet, useLocation } from "react-router-dom"
import { useAuth } from "@/hooks/useAuth"

export function ProtectedRoute() {
  const { user } = useAuth()
  const location = useLocation()

  // Remember where they were heading, so login can send them back there.
  if (!user) return <Navigate to="/login" replace state={{ from: location }} />
  return <Outlet />
}