const apiUrl = import.meta.env.VITE_API_URL as string | undefined

if (!apiUrl) {
  throw new Error("VITE_API_URL is not set. Copy .env.example to .env.")
}

export const config = {
  apiUrl: apiUrl.replace(/\/$/, ""), // strip a trailing slash
}