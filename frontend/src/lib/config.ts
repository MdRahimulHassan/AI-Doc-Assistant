const apiUrl = import.meta.env.VITE_API_URL as string | undefined

if (!apiUrl) {
  throw new Error("VITE_API_URL is not set. Copy .env.example to .env.")
}

// Must match MAX_UPLOAD_MB on the backend. This copy only powers instant feedback;
// the server stays the authority.
const parsedMaxUploadMb = Number(import.meta.env.VITE_MAX_UPLOAD_MB ?? 10)
const maxUploadMb =
  Number.isFinite(parsedMaxUploadMb) && parsedMaxUploadMb > 0 ? parsedMaxUploadMb : 10

export const config = {
  apiUrl: apiUrl.replace(/\/$/, ""),
  maxUploadMb,
  maxUploadBytes: maxUploadMb * 1024 * 1024,
}