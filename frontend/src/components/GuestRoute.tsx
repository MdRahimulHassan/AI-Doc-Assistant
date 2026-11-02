import { Navigate, Outlet, useLocation, type Location } from "react-router-dom"
import { useAuth } from "@/hooks/useAuth"

export function GuestRoute() {
  const { user } = useAuth()
  const location = useLocation()

  if (user) {
    const from = (location.state as { from?: Location } | null)?.from
    const target = from ? `${from.pathname}${from.search}${from.hash}` : "/"
    return <Navigate to={target} replace />
  }
  return <Outlet />
}