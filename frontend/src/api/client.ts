import axios, { type AxiosError, type AxiosRequestConfig } from "axios"
import { config } from "@/lib/config"
import type { ApiEnvelope } from "@/types/api"
import {
  ApiError,
  ErrorCode,
  canceledError,
  errorFromResponse,
  networkError,
  timeoutError,
  unknownError,
} from "./errors"
import { clearToken, getToken, notifyUnauthorized } from "./session"

export const client = axios.create({
  baseURL: config.apiUrl,
  timeout: 30_000, // uploads override this per call
  headers: { Accept: "application/json" },
})

// ---- Request: attach the JWT ------------------------------------------------
client.interceptors.request.use((request) => {
  const token = getToken()
  if (token) request.headers.set("Authorization", `Bearer ${token}`)
  return request
})

// ---- Response: unwrap the envelope, normalise every failure ------------------
function header(headers: unknown, name: string): string | undefined {
  const value = (headers as Record<string, unknown> | undefined)?.[name]
  return typeof value === "string" ? value : undefined
}

/**
 * A 401 only means "your session is over" when the auth middleware said so
 * (code UNAUTHORIZED) AND the failing request carried the token we still hold.
 * A wrong password is also a 401 (INVALID_CREDENTIALS) and must NOT log anyone
 * out, and a late response to an old request must not kill a fresh login.
 */
function expireSessionIfNeeded(error: AxiosError, apiError: ApiError): void {
  if (apiError.status !== 401 || apiError.code !== ErrorCode.Unauthorized) return
  const current = getToken()
  const sent = error.config?.headers?.get("Authorization")
  if (current && sent === `Bearer ${current}`) {
    clearToken()
    notifyUnauthorized()
  }
}

function toApiError(error: unknown): ApiError {
  if (error instanceof ApiError) return error
  if (axios.isCancel(error)) return canceledError()
  if (!axios.isAxiosError(error)) return unknownError()

  const res = error.response
  if (res) {
    const apiError = errorFromResponse(res.status, res.data, {
      retryAfter: header(res.headers, "retry-after"),
      requestId: header(res.headers, "x-request-id"),
    })
    expireSessionIfNeeded(error, apiError)
    return apiError
  }

  if (error.code === "ECONNABORTED" || error.code === "ETIMEDOUT") return timeoutError()
  return networkError() // request was sent, nothing came back
}

client.interceptors.response.use(
  (response) => {
    // 204 (delete) has no body.
    if (response.status === 204) {
      response.data = undefined
      return response
    }
    const body = response.data as ApiEnvelope<unknown> | undefined
    if (body && typeof body === "object" && body.success === true) {
      response.data = body.data // from here on, .data is the payload itself
      return response
    }
    // A 2xx that is not our envelope is a bug or a misconfigured proxy.
    throw errorFromResponse(response.status, body, {
      requestId: header(response.headers, "x-request-id"),
    })
  },
  (error: unknown) => Promise.reject(toApiError(error)),
)

// ---- Typed helpers: the only thing the resource files use -------------------
// The interceptor already replaced response.data with the unwrapped payload,
// so `.data` really is a T here.
export const http = {
  async get<T>(url: string, options?: AxiosRequestConfig): Promise<T> {
    return (await client.get<T>(url, options)).data
  },
  async post<T>(url: string, body?: unknown, options?: AxiosRequestConfig): Promise<T> {
    return (await client.post<T>(url, body, options)).data
  },
  async delete(url: string, options?: AxiosRequestConfig): Promise<void> {
    await client.delete(url, options)
  },
}