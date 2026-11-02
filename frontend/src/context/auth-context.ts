import { createContext } from "react"
import type { Credentials } from "@/api"
import type { User } from "@/types/api"

export type AuthStatus = "loading" | "authenticated" | "unauthenticated" | "error"

export interface AuthContextValue {
  user: User | null
  status: AuthStatus
  login: (credentials: Credentials) => Promise<void>
  /** Registers, then signs in. signedIn is false if only the auto-login step failed. */
  register: (credentials: Credentials) => Promise<{ signedIn: boolean }>
  logout: () => void
  /** Re-runs the startup check after status === "error". */
  retry: () => void
}

export const AuthContext = createContext<AuthContextValue | null>(null)