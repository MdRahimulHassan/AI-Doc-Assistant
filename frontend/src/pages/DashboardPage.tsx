import type { ReactNode } from "react"
import { AlertCircle, FileText, RefreshCw } from "lucide-react"
import { errorMessage } from "@/api"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { DocumentCard, DocumentCardSkeleton } from "@/components/DocumentCard"
import { EmptyState } from "@/components/EmptyState"
import { ErrorState } from "@/components/ErrorState"
import { UploadDropzone } from "@/components/UploadDropzone"
import { LIST_LIMIT, useDocuments } from "@/hooks/useDocuments"

export default function DashboardPage() {
  const { data, isPending, isError, error, isFetching, refetch } = useDocuments()
  const documents = data?.documents ?? []
  const processing = documents.filter((doc) => doc.status === "processing").length

  let content: ReactNode
  if (isPending) {
    content = (
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3" aria-busy="true">
        {Array.from({ length: 6 }, (_, i) => (
          <DocumentCardSkeleton key={i} />
        ))}
      </div>
    )
  } else if (!data) {
    // Not pending and no data: the first load failed.
    content = (
      <ErrorState
        title="Couldn’t load your documents"
        message={errorMessage(error)}
        onRetry={() => void refetch()}
        retrying={isFetching}
      />
    )
  } else if (documents.length === 0) {
    content = (
      <EmptyState
        icon={FileText}
        title="No documents yet"
        description="Upload your first document above to get a summary and start asking questions about it."
      />
    )
  } else {
    content = (
      <div className="space-y-4">
        {isError && (
          // A background refresh failed, but we still have data worth showing.
          <Alert variant="destructive" role="alert">
            <AlertCircle />
            <AlertDescription className="flex items-center justify-between gap-4">
              <span>Couldn’t refresh your documents.</span>
              <Button size="sm" variant="outline" onClick={() => void refetch()} disabled={isFetching}>
                Try again
              </Button>
            </AlertDescription>
          </Alert>
        )}
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {documents.map((doc) => (
            <DocumentCard key={doc.id} doc={doc} />
          ))}
        </div>
        {documents.length >= LIST_LIMIT && (
          <p className="text-xs text-muted-foreground">
            Showing your {LIST_LIMIT} most recent documents.
          </p>
        )}
      </div>
    )
  }

  return (
    <div className="space-y-8">
      <header className="flex items-start justify-between gap-4">
        <div className="space-y-1">
          <h1 className="text-2xl font-semibold tracking-tight">Your documents</h1>
          <p className="text-sm text-muted-foreground">
            Upload a document, then summarize it or ask questions about it.
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
          <RefreshCw className={isFetching ? "animate-spin" : undefined} />
          <span className="hidden sm:inline">Refresh</span>
        </Button>
      </header>

      <UploadDropzone />

      <section aria-labelledby="library-heading" className="space-y-4">
        <div className="flex items-baseline justify-between gap-4">
          <h2 id="library-heading" className="text-lg font-semibold">
            Library
          </h2>
          {data && documents.length > 0 && (
            <span className="text-sm text-muted-foreground">
              {documents.length} {documents.length === 1 ? "document" : "documents"}
              {processing > 0 && ` · ${processing} processing`}
            </span>
          )}
        </div>
        {content}
      </section>
    </div>
  )
}