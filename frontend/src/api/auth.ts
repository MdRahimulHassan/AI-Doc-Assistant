import type { LoginResponse, User } from "@/types/api"
import { http } from "./client"

export interface Credentials {
  email: string
  password: string
}

// Deliberately does NOT store the token: that is AuthContext's job (Step 4).
export const authApi = {
  register: async (credentials: Credentials): Promise<User> =>
    (await http.post<{ user: User }>("/api/auth/register", credentials)).user,

  login: (credentials: Credentials): Promise<LoginResponse> =>
    http.post<LoginResponse>("/api/auth/login", credentials),

  me: async (signal?: AbortSignal): Promise<User> =>
    (await http.get<{ user: User }>("/api/me", { signal })).user,
}