import type { Document, DocumentList } from "@/types/api"
import { http } from "./client"

export interface UploadOptions {
  onProgress?: (percent: number) => void
  signal?: AbortSignal
}

export const documentsApi = {
  list: (params: { limit?: number; offset?: number } = {}, signal?: AbortSignal) =>
    http.get<DocumentList>("/api/documents", { params, signal }),

  get: async (id: string, signal?: AbortSignal): Promise<Document> =>
    (await http.get<{ document: Document }>(`/api/documents/${id}`, { signal })).document,

  upload: async (file: File, { onProgress, signal }: UploadOptions = {}): Promise<Document> => {
    const form = new FormData()
    form.append("file", file) // the backend reads the field named "file"

    // Do NOT set Content-Type by hand: the browser must add the multipart boundary.
    const res = await http.post<{ document: Document }>("/api/documents", form, {
      timeout: 120_000,
      signal,
      onUploadProgress: (event) => {
        if (event.total) onProgress?.(Math.round((event.loaded / event.total) * 100))
      },
    })
    return res.document
  },

  remove: (id: string): Promise<void> => http.delete(`/api/documents/${id}`),
}