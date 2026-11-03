import { useMutation, useQueryClient } from "@tanstack/react-query"
import { ApiError, ErrorCode, assistantApi } from "@/api"
import { documentKeys } from "@/hooks/useDocuments"

export function useSummarize(documentId: string) {
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: documentKeys.detail(documentId) })

  return useMutation({
    mutationFn: () => assistantApi.summarize(documentId),
    // Refetch rather than trust the 202 body: the detail query then sees
    // summary_status "processing" and starts polling by itself.
    onSuccess: refresh,
    onError: (error) => {
      // Someone (another tab) already started it: show real progress instead of an error.
      if (error instanceof ApiError && error.is(ErrorCode.SummaryInProgress)) return refresh()
    },
  })
}
