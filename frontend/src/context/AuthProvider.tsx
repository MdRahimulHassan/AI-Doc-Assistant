import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react"
import { useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import {
  ApiError,
  ErrorCode,
  authApi,
  clearToken,
  getToken,
  setToken,
  setUnauthorizedHandler,
  type Credentials,
} from "@/api"
import type { User } from "@/types/api"
import { AuthContext, type AuthContextValue, type AuthStatus } from "./auth-context"

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient()
  const [user, setUser] = useState<User | null>(null)
  // A stored token means "unknown until the server confirms it". No token means logged out.
  const [status, setStatus] = useState<AuthStatus>(() =>
    getToken() ? "loading" : "unauthenticated",
  )
  const [attempt, setAttempt] = useState(0)

  const resetSession = useCallback(() => {
    clearToken()
    setUser(null)
    setStatus("unauthenticated")
    queryClient.clear() // the next person to sign in must never see cached data
  }, [queryClient])

  // 1) The API layer calls this when the server rejects our token.
  useEffect(() => {
    setUnauthorizedHandler(() => {
      resetSession()
      toast.info("Your session has expired. Please sign in again.")
    })
    return () => setUnauthorizedHandler(null)
  }, [resetSession])

  // 2) On startup (and on retry), ask the server whether the stored token is still good.
  useEffect(() => {
    if (!getToken()) return

    const controller = new AbortController()
    authApi
      .me(controller.signal)
      .then((me) => {
        setUser(me)
        setStatus("authenticated")
      })
      .catch((error: unknown) => {
        if (!(error instanceof ApiError)) return setStatus("error")
        if (error.is(ErrorCode.Canceled)) return // StrictMode's first run, or unmount
        if (error.is(ErrorCode.Unauthorized)) return // handled above: token cleared, state reset
        setStatus("error") // network down or 5xx: keep the token and offer a retry
      })

    return () => controller.abort()
  }, [attempt])

  const login = useCallback(async (credentials: Credentials) => {
    const result = await authApi.login(credentials)
    setToken(result.token) // storage first, so the very next request is authenticated
    setUser(result.user)
    setStatus("authenticated")
  }, [])

  const register = useCallback(
    async (credentials: Credentials) => {
      await authApi.register(credentials) // errors (EMAIL_TAKEN, validation) propagate to the form
      try {
        await login(credentials)
        return { signedIn: true }
      } catch {
        return { signedIn: false }
      }
    },
    [login],
  )

  const retry = useCallback(() => {
    setStatus("loading")
    setAttempt((n) => n + 1)
  }, [])

  const value = useMemo<AuthContextValue>(
    () => ({ user, status, login, register, logout: resetSession, retry }),
    [user, status, login, register, resetSession, retry],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}