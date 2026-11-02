import type { FileRejection } from "react-dropzone"
import { ApiError, ErrorCode } from "@/api"
import { config } from "@/lib/config"
import { sentence } from "@/lib/text"

export const ACCEPTED_TYPES = {
  "application/pdf": [".pdf"],
  "application/vnd.openxmlformats-officedocument.wordprocessingml.document": [".docx"],
  "text/plain": [".txt"],
}

export const MAX_UPLOAD_LABEL = `${config.maxUploadMb} MB`

/** Short label for the file-type tile on a card. */
export function fileLabel(mimeType: string): string {
  if (mimeType === "application/pdf") return "PDF"
  if (mimeType.includes("wordprocessingml")) return "DOCX"
  if (mimeType.startsWith("text/")) return "TXT"
  return "FILE"
}

/** Client-side rejection: shown instantly, before any network request. */
export function describeRejection(rejection: FileRejection): string {
  const name = rejection.file.name
  switch (rejection.errors[0]?.code) {
    case "file-too-large":
      return `“${name}” is too large. The maximum size is ${MAX_UPLOAD_LABEL}.`
    case "file-too-small":
      return `“${name}” is empty. Choose a file that has content.`
    case "file-invalid-type":
      return `“${name}” isn’t supported. Upload a PDF, DOCX or TXT file.`
    case "too-many-files":
      return "Upload one file at a time."
    default:
      return rejection.errors[0]?.message ?? "That file can’t be uploaded."
  }
}

/** Server-side failure. Returns null for a user cancel: nothing to show. */
export function describeUploadError(error: unknown): string | null {
  if (!(error instanceof ApiError)) return "The upload failed. Please try again."

  switch (error.code) {
    case ErrorCode.Canceled:
      return null
    case ErrorCode.FileTooLarge:
      return sentence(error.message) // the server knows its own limit
    case ErrorCode.UnsupportedFileType:
      return "That file type isn’t supported. Upload a PDF, DOCX or TXT file whose extension matches its content."
    case ErrorCode.NoFile:
      return "That file is empty or wasn’t received. Choose a file that has content."
    case ErrorCode.RateLimited:
      return error.retryAfter
        ? `You’re uploading too quickly. Try again in ${error.retryAfter} seconds.`
        : "You’re uploading too quickly. Please wait a moment."
    case ErrorCode.Unavailable:
      return "The server is busy processing documents. Please try again shortly."
    default:
      return error.message
  }
}