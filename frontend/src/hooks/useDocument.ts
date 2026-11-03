import { useRef } from "react"
import { useQuery } from "@tanstack/react-query"
import { documentsApi } from "@/api"
import { documentKeys } from "@/hooks/useDocuments"

const POLL_INTERVAL_MS = 3_000
const GIVE_UP_AFTER_MS = 10 * 60_000

/** One document, polled while it (or its summary) is being worked on. */
export function useDocument(id: string) {
  const busySince = useRef<number | null>(null)

  return useQuery({
    queryKey: documentKeys.detail(id),
    queryFn: ({ signal }) => documentsApi.get(id, signal),
    enabled: id !== "",
    refetchInterval: (query) => {
      const doc = query.state.data
      const busy = !!doc && (doc.status === "processing" || doc.summary_status === "processing")
      if (!busy) {
        busySince.current = null
        return false
      }
      busySince.current ??= Date.now()
      return Date.now() - busySince.current < GIVE_UP_AFTER_MS ? POLL_INTERVAL_MS : false
    },
    refetchOnWindowFocus: true,
  })
}