import { AlertCircle, Info, Loader2, RefreshCw, Sparkles, type LucideIcon } from "lucide-react"
import { ApiError, ErrorCode } from "@/api"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Card } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { CooldownButton } from "@/components/CooldownButton"
import { Markdown } from "@/components/Markdown"
import { useSummarize } from "@/hooks/useSummarize"
import { formatRelativeTime } from "@/lib/format"
import { sentence } from "@/lib/text"
import type { Document } from "@/types/api"

function describeSummaryError(error: unknown): { message: string; tone: "info" | "error" } {
  if (!(error instanceof ApiError)) {
    return { message: "Couldn’t start the summary. Please try again.", tone: "error" }
  }
  switch (error.code) {
    case ErrorCode.SummaryInProgress:
      return { message: "A summary is already being written. It will appear here as soon as it’s ready.", tone: "info" }
    case ErrorCode.DocumentTooLong:
      return { message: "This document is too long to summarize. You can still ask questions about it in the chat.", tone: "error" }
    case ErrorCode.DocumentNotReady:
      return { message: "This document hasn’t finished processing yet. Try again in a moment.", tone: "error" }
    case ErrorCode.RateLimited:
      return { message: "You’re requesting summaries too quickly. Please wait a moment.", tone: "error" }
    case ErrorCode.Unavailable:
      return { message: "The server is busy writing other summaries. Please try again shortly.", tone: "error" }
    default:
      return { message: sentence(error.message), tone: "error" }
  }
}

export function SummaryPanel({ doc }: { doc: Document }) {
  const summarize = useSummarize(doc.id)
  const apiError = summarize.error instanceof ApiError ? summarize.error : null

  const docReady = doc.status === "ready"
  const isWriting = doc.summary_status === "processing"
  const tooLong = apiError?.is(ErrorCode.DocumentTooLong) ?? false
  const problem = summarize.error && !isWriting ? describeSummaryError(summarize.error) : null

  // `key` restarts the Retry-After countdown on every new attempt.
  const action = (label: string, Icon: LucideIcon, variant: "default" | "outline" = "default") => (
    <CooldownButton
      key={summarize.submittedAt}
      size="sm"
      variant={variant}
      cooldown={apiError?.retryAfter}
      disabled={!docReady || tooLong || summarize.isPending}
      onClick={() => summarize.mutate()}
    >
      {summarize.isPending ? <Loader2 className="animate-spin" /> : <Icon />}
      {label}
    </CooldownButton>
  )

  let body
  if (!docReady) {
    body = (
      <p className="text-sm text-muted-foreground">
        Summaries are available once this document has finished processing.
      </p>
    )
  } else if (isWriting) {
    body = (
      <div role="status" className="space-y-4">
        <div className="flex items-center gap-3 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" /> Writing your summary…
        </div>
        <div className="space-y-2">
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-11/12" />
          <Skeleton className="h-4 w-4/5" />
        </div>
        <p className="text-xs text-muted-foreground">
          Long documents are summarized in parts, so this can take a minute. You can keep chatting meanwhile.
        </p>
      </div>
    )
  } else if (doc.summary_status === "ready" && doc.summary) {
    body = <Markdown className="text-sm">{doc.summary}</Markdown>
  } else if (doc.summary_status === "failed") {
    body = (
      <div className="space-y-4">
        <Alert variant="destructive">
          <AlertCircle />
          <AlertDescription>
            {sentence(doc.summary_error ?? "The summary couldn’t be written.")}
          </AlertDescription>
        </Alert>
        {action("Try again", RefreshCw, "outline")}
      </div>
    )
  } else {
    body = (
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">
          Get a short overview of this document. You can ask detailed questions in the chat at any time.
        </p>
        {action("Generate summary", Sparkles)}
      </div>
    )
  }

  const showRegenerate = docReady && doc.summary_status === "ready"

  return (
    <Card className="h-full min-h-0 gap-0 py-0">
      <div className="flex items-center justify-between gap-3 border-b border-border px-6 py-4">
        <div>
          <h2 className="text-sm font-semibold">Summary</h2>
          {showRegenerate && doc.summary_updated_at && (
            <p className="text-xs text-muted-foreground">
              Updated {formatRelativeTime(doc.summary_updated_at)}
            </p>
          )}
        </div>
        {showRegenerate && action("Regenerate", RefreshCw, "outline")}
      </div>

      <div className="min-h-0 flex-1 space-y-4 overflow-y-auto px-6 py-6">
        {problem && (
          <Alert variant={problem.tone === "info" ? "default" : "destructive"} role="alert">
            {problem.tone === "info" ? <Info /> : <AlertCircle />}
            <AlertDescription>{problem.message}</AlertDescription>
          </Alert>
        )}
        {body}
      </div>
    </Card>
  )
}