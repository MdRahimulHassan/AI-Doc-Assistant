import { Link, useParams } from "react-router-dom"
import { AlertCircle, ArrowLeft, FileQuestion, Info } from "lucide-react"
import { ApiError, ErrorCode, errorMessage } from "@/api"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { ChatWindow } from "@/components/ChatWindow"
import { EmptyState } from "@/components/EmptyState"
import { ErrorState } from "@/components/ErrorState"
import { StatusBadge } from "@/components/StatusBadge"
import { SummaryPanel } from "@/components/SummaryPanel"
import { useDocument } from "@/hooks/useDocument"
import { formatFileSize, formatRelativeTime } from "@/lib/format"
import { sentence } from "@/lib/text"
import type { ReactNode } from "react"

function BackLink() {
  return (
    <Button variant="ghost" size="sm" className="-ml-3">
      <Link to="/">
        <ArrowLeft /> All documents
      </Link>
    </Button>
  )
}

// The panes need a definite height so each can scroll inside itself.
function Pane({ children }: { children: ReactNode }) {
  return <div className="h-[calc(100dvh-20rem)] min-h-[26rem] lg:h-[calc(100dvh-16rem)]">{children}</div>
}

export default function DocumentPage() {
  const { id = "" } = useParams()
  const { data: doc, isPending, error, refetch, isFetching } = useDocument(id)

  if (isPending) {
    return (
      <div className="space-y-6" aria-busy="true">
        <Skeleton className="h-8 w-40" />
        <Skeleton className="h-8 w-2/3" />
        <Skeleton className="h-[28rem] w-full rounded-xl" />
      </div>
    )
  }

  if (!doc) {
    const notFound = error instanceof ApiError && error.is(ErrorCode.NotFound)
    return (
      <div className="space-y-6">
        <BackLink />
        {notFound ? (
          <EmptyState
            icon={FileQuestion}
            title="Document not found"
            description="It may have been deleted, or it belongs to another account."
            action={
              <Button>
                <Link to="/">Back to documents</Link>
              </Button>
            }
          />
        ) : (
          <ErrorState
            title="Couldn’t load this document"
            message={errorMessage(error)}
            onRetry={() => void refetch()}
            retrying={isFetching}
          />
        )}
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <BackLink />
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="min-w-0 truncate text-2xl font-semibold tracking-tight" title={doc.filename}>
            {doc.filename}
          </h1>
          <StatusBadge status={doc.status} />
        </div>
        <p className="text-sm text-muted-foreground">
          {formatFileSize(doc.size_bytes)} · uploaded {formatRelativeTime(doc.created_at)}
        </p>
      </div>

      {doc.status === "processing" && (
        <Alert>
          <Info />
          <AlertDescription>
            This document is still being processed. The page updates by itself, and chat and summaries unlock when it’s ready.
          </AlertDescription>
        </Alert>
      )}
      {doc.status === "failed" && (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertDescription>
            {sentence(doc.error_message ?? "This document couldn’t be processed.")}
          </AlertDescription>
        </Alert>
      )}

      {/* One tree for every screen size: tabs on small screens, side by side from lg up. */}
      <Tabs defaultValue="chat" className="gap-4 lg:grid lg:grid-cols-5 lg:items-start lg:gap-6">
        <TabsList className="grid w-full grid-cols-2 lg:hidden">
          <TabsTrigger value="summary">Summary</TabsTrigger>
          <TabsTrigger value="chat">Chat</TabsTrigger>
        </TabsList>

        <TabsContent value="summary" keepMounted className="lg:col-span-2 lg:block">
          <Pane>
            <SummaryPanel key={doc.id} doc={doc} />
          </Pane>
        </TabsContent>
        <TabsContent value="chat" keepMounted className="lg:col-span-3 lg:block">
          <Pane>
            <ChatWindow key={doc.id} doc={doc} />
          </Pane>
        </TabsContent>
      </Tabs>
    </div>
  )
}