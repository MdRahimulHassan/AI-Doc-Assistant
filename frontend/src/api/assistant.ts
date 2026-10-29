import type { Document, MessagePage } from "@/types/api"
import { http } from "./client"

// The streaming `ask` call is added in Step 6. It can't use axios (we need to
// read the response body as it arrives), but it will reuse errorFromResponse()
// and getToken() so errors look identical.
export const assistantApi = {
  /** 202: queued. Poll documentsApi.get() until summary_status is ready or failed. */
  summarize: async (documentId: string): Promise<Document> =>
    (await http.post<{ document: Document }>(`/api/documents/${documentId}/summarize`)).document,

  messages: (
    documentId: string,
    params: { limit?: number; before?: number } = {},
    signal?: AbortSignal,
  ) => http.get<MessagePage>(`/api/documents/${documentId}/messages`, { params, signal }),
}