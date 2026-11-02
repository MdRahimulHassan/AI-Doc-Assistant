import { useCallback, useRef, useState } from "react"
import { useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { ApiError, ErrorCode, documentsApi, errorMessage } from "@/api"
import type { DocumentList } from "@/types/api"

export const documentKeys = {
  all: ["documents"] as const,
  list: () => [...documentKeys.all, "list"] as const,
  detail: (id: string) => [...documentKeys.all, "detail", id] as const, // used in Step 6
}

export const LIST_LIMIT = 100
const POLL_INTERVAL_MS = 3_000
const GIVE_UP_AFTER_MS = 10 * 60_000

/**
 * Poll only while something is genuinely being processed. A document that has
 * been "processing" for 10+ minutes is treated as stuck, so a crashed worker
 * can't keep every open tab polling forever.
 */
function shouldPoll(data: DocumentList | undefined): boolean {
  if (!data) return false
  const now = Date.now()
  return data.documents.some(
    (doc) =>
      doc.status === "processing" && now - new Date(doc.created_at).getTime() < GIVE_UP_AFTER_MS,
  )
}

export function useDocuments() {
  return useQuery({
    queryKey: documentKeys.list(),
    queryFn: ({ signal }) => documentsApi.list({ limit: LIST_LIMIT }, signal),
    // Returning false switches polling off; TanStack also pauses it while the tab is hidden.
    refetchInterval: (query) => (shouldPoll(query.state.data) ? POLL_INTERVAL_MS : false),
    refetchOnWindowFocus: true,
  })
}

export function useUploadDocument() {
  const queryClient = useQueryClient()
  const [progress, setProgress] = useState(0)
  const controllerRef = useRef<AbortController | null>(null)

  const mutation = useMutation({
    mutationFn: (file: File) => {
      const controller = new AbortController()
      controllerRef.current = controller
      setProgress(0)
      return documentsApi.upload(file, { signal: controller.signal, onProgress: setProgress })
    },
    onSuccess: (document) => {
      toast.success(`Uploaded “${document.filename}”. Processing it now…`)
      // Returning the promise keeps the upload "pending" until the new card is on screen.
      return queryClient.invalidateQueries({ queryKey: documentKeys.list() })
    },
    onSettled: () => {
      controllerRef.current = null
    },
  })

  const cancel = useCallback(() => controllerRef.current?.abort(), [])

  return {
    upload: mutation.mutate,
    reset: mutation.reset,
    cancel,
    isUploading: mutation.isPending,
    error: mutation.error,
    file: mutation.variables,
    progress,
  }
}

function removeFromCache(queryClient: QueryClient, id: string) {
  queryClient.setQueryData<DocumentList>(
    documentKeys.list(),
    (old) => old && { ...old, documents: old.documents.filter((doc) => doc.id !== id) },
  )
  queryClient.removeQueries({ queryKey: documentKeys.detail(id) })
}

export function useDeleteDocument() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => documentsApi.remove(id),
    onSuccess: (_data, id) => {
      removeFromCache(queryClient, id)
      toast.success("Document deleted.")
    },
    onError: (error, id) => {
      if (error instanceof ApiError && error.is(ErrorCode.NotFound)) {
        removeFromCache(queryClient, id) // already gone (another tab?): the goal is met
        toast.info("That document was already deleted.")
        return
      }
      toast.error(errorMessage(error, "Couldn’t delete the document. Please try again."))
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: documentKeys.list() }),
  })
}