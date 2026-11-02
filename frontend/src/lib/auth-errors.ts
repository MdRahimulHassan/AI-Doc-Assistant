import { ApiError, ErrorCode } from "@/api"
import { sentence } from "@/lib/text"

export type AuthField = "email" | "password" | "confirmPassword"
export type FieldErrors = Partial<Record<AuthField, string>>

export interface MappedAuthError {
  fields: FieldErrors
  form?: string
}

export function mapAuthError(error: unknown): MappedAuthError {
  if (!(error instanceof ApiError)) {
    return { fields: {}, form: "Something went wrong. Please try again." }
  }

  switch (error.code) {
    case ErrorCode.InvalidCredentials:
      return { fields: {}, form: "Incorrect email or password." }

    case ErrorCode.EmailTaken:
      return { fields: { email: "An account with this email already exists." } }

    case ErrorCode.Validation: {
      const server = error.fieldErrors
      const fields: FieldErrors = {}
      if (server.email) fields.email = sentence(server.email)
      if (server.password) fields.password = sentence(server.password)
      // If the server named a field we don't have an input for, don't swallow it.
      return Object.keys(fields).length > 0 ? { fields } : { fields: {}, form: error.message }
    }

    case ErrorCode.RateLimited:
      return {
        fields: {},
        form: error.retryAfter
          ? `Too many attempts. Try again in ${error.retryAfter} seconds.`
          : "Too many attempts. Please wait a moment and try again.",
      }

    default:
      // Network, timeout, 5xx: the ApiError message is already user-friendly.
      return { fields: {}, form: error.message }
  }
}