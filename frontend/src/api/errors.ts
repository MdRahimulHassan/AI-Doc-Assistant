import type { ApiEnvelope } from "@/types/api"

/** Codes the UI branches on. Backend codes mirror pkg/response; the last four are client-side. */
export const ErrorCode = {
  Validation: "VALIDATION_ERROR",
  Unauthorized: "UNAUTHORIZED",
  InvalidCredentials: "INVALID_CREDENTIALS",
  EmailTaken: "EMAIL_TAKEN",
  NotFound: "NOT_FOUND",
  FileTooLarge: "FILE_TOO_LARGE",
  UnsupportedFileType: "UNSUPPORTED_FILE_TYPE",
  NoFile: "NO_FILE",
  DocumentNotReady: "DOCUMENT_NOT_READY",
  AnswerInProgress: "ANSWER_IN_PROGRESS",
  RateLimited: "RATE_LIMITED",
  LLMUnavailable: "LLM_UNAVAILABLE",
  LLMBusy: "LLM_BUSY",
  LLMTimeout: "LLM_TIMEOUT",
  LLMError: "LLM_ERROR",
  SummaryInProgress: "SUMMARY_IN_PROGRESS",
  DocumentTooLong: "DOCUMENT_TOO_LONG",
  Unavailable: "SERVICE_UNAVAILABLE",
  Internal: "INTERNAL_ERROR",
  // Produced by the client, never by the server:
  Network: "NETWORK_ERROR",
  Timeout: "TIMEOUT",
  Canceled: "CANCELED",
  Unknown: "UNKNOWN",
} as const

export interface ApiErrorInit {
  code: string
  message: string
  status: number // 0 when no HTTP response exists (network, timeout, cancel)
  details?: unknown
  retryAfter?: number // seconds, from the Retry-After header
  requestId?: string // X-Request-ID, handy when reading server logs
}

export class ApiError extends Error {
  readonly code: string
  readonly status: number
  readonly details: unknown
  readonly retryAfter: number | undefined
  readonly requestId: string | undefined

  constructor(init: ApiErrorInit) {
    super(init.message)
    this.name = "ApiError"
    this.code = init.code
    this.status = init.status
    this.details = init.details
    this.retryAfter = init.retryAfter
    this.requestId = init.requestId
  }

  /** error.is(ErrorCode.NotFound, ErrorCode.Unauthorized) */
  is(...codes: string[]): boolean {
    return codes.includes(this.code)
  }

  /** VALIDATION_ERROR details as { email: "...", password: "..." } for forms. */
  get fieldErrors(): Record<string, string> {
    const out: Record<string, string> = {}
    if (!Array.isArray(this.details)) return out
    for (const item of this.details) {
      if (
        item &&
        typeof item.field === "string" &&
        typeof item.message === "string" &&
        !(item.field in out)
      ) {
        out[item.field] = item.message
      }
    }
    return out
  }
}

export function isApiError(value: unknown): value is ApiError {
  return value instanceof ApiError
}

/** Safe text for a toast or banner, whatever was thrown. */
export function errorMessage(
  error: unknown,
  fallback = "Something went wrong. Please try again.",
): string {
  return error instanceof ApiError ? error.message : fallback
}

function parseRetryAfter(value: string | null | undefined): number | undefined {
  if (value == null) return undefined
  const n = Number(value)
  return Number.isFinite(n) && n > 0 ? Math.ceil(n) : undefined
}

/**
 * Builds an ApiError from an HTTP response. Used by the axios client now and
 * by the streaming fetch in Step 6, so both fail in exactly the same way.
 */
export function errorFromResponse(
  status: number,
  body: unknown,
  meta: { retryAfter?: string | null; requestId?: string | null } = {},
): ApiError {
  const retryAfter = parseRetryAfter(meta.retryAfter)
  const requestId = meta.requestId ?? undefined

  const envelope = body as Partial<ApiEnvelope<unknown>> | null | undefined
  const err = envelope && typeof envelope === "object" ? envelope.error : undefined

  if (err && typeof err.code === "string" && typeof err.message === "string") {
    return new ApiError({
      code: err.code,
      message: err.message,
      status,
      details: err.details,
      retryAfter,
      requestId,
    })
  }

  // Not our envelope: a proxy error page, a crash, an unexpected 2xx body.
  return new ApiError({
    code: ErrorCode.Unknown,
    message:
      status >= 500
        ? "The server had a problem. Please try again in a moment."
        : `Unexpected response from the server (HTTP ${status}).`,
    status,
    retryAfter,
    requestId,
  })
}

export const networkError = () =>
  new ApiError({
    code: ErrorCode.Network,
    status: 0,
    message: "Can't reach the server. Check your connection and try again.",
  })

export const timeoutError = () =>
  new ApiError({
    code: ErrorCode.Timeout,
    status: 0,
    message: "The request took too long. Please try again.",
  })

export const canceledError = () =>
  new ApiError({ code: ErrorCode.Canceled, status: 0, message: "Request canceled." })

export const unknownError = () =>
  new ApiError({
    code: ErrorCode.Unknown,
    status: 0,
    message: "Something went wrong. Please try again.",
  })