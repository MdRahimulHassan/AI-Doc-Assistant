const apiUrl = import.meta.env.VITE_API_URL as string | undefined

if (!apiUrl) {
  throw new Error("VITE_API_URL is not set. Copy .env.example to .env.")
}

function positiveNumber(raw: unknown, fallback: number): number {
  const n = Number(raw ?? fallback)
  return Number.isFinite(n) && n > 0 ? n : fallback
}

// These two must match the backend (MAX_UPLOAD_MB, ASK_MAX_QUESTION_CHARS).
// They only power instant feedback; the server stays the authority.
const maxUploadMb = positiveNumber(import.meta.env.VITE_MAX_UPLOAD_MB, 10)

export const config = {
  apiUrl: apiUrl.replace(/\/$/, ""),
  maxUploadMb,
  maxUploadBytes: maxUploadMb * 1024 * 1024,
  maxQuestionChars: positiveNumber(import.meta.env.VITE_MAX_QUESTION_CHARS, 1000),
}