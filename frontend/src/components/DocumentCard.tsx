import { useState } from "react"
import { Link } from "react-router-dom"
import { Loader2, MoreHorizontal, Trash2 } from "lucide-react"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Skeleton } from "@/components/ui/skeleton"
import { StatusBadge } from "@/components/StatusBadge"
import { useDeleteDocument } from "@/hooks/useDocuments"
import { fileLabel } from "@/lib/files"
import { formatDateTime, formatFileSize, formatRelativeTime } from "@/lib/format"
import { sentence } from "@/lib/text"
import { cn } from "@/lib/utils"
import type { Document } from "@/types/api"

export function DocumentCard({ doc }: { doc: Document }) {
  const [confirmOpen, setConfirmOpen] = useState(false)
  const remove = useDeleteDocument()
  const isReady = doc.status === "ready"

  return (
    <>
      <Card
        className={cn(
          "relative gap-4 transition-shadow focus-within:ring-[3px] focus-within:ring-ring/50",
          isReady && "hover:shadow-md",
        )}
      >
        <CardContent className="space-y-4">
          <div className="flex items-start gap-3">
            <span className="grid size-10 shrink-0 place-items-center rounded-lg bg-muted text-xs font-semibold text-muted-foreground">
              {fileLabel(doc.mime_type)}
            </span>

            <div className="min-w-0 flex-1">
              {isReady ? (
                // "Stretched link": the ::after covers the whole card, so the card is one big click target.
                <Link
                  to={`/documents/${doc.id}`}
                  title={doc.filename}
                  className="block truncate font-medium outline-none after:absolute after:inset-0"
                >
                  {doc.filename}
                </Link>
              ) : (
                <p className="truncate font-medium" title={doc.filename}>
                  {doc.filename}
                </p>
              )}
              <p className="text-xs text-muted-foreground">
                {formatFileSize(doc.size_bytes)} ·{" "}
                <time dateTime={doc.created_at} title={formatDateTime(doc.created_at)}>
                  {formatRelativeTime(doc.created_at)}
                </time>
              </p>
            </div>

            <DropdownMenu>
              <DropdownMenuTrigger>
                <Button
                  variant="ghost"
                  size="icon"
                  className="relative z-10 -mt-1 -mr-2 size-8 shrink-0"
                  aria-label={`Actions for ${doc.filename}`}
                >
                  <MoreHorizontal className="size-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem
                  className="text-destructive focus:text-destructive"
                  onSelect={() => setConfirmOpen(true)}
                >
                  <Trash2 /> Delete
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>

          <StatusBadge status={doc.status} />

          {doc.status === "failed" && doc.error_message && (
            <p className="line-clamp-2 text-xs text-destructive" title={doc.error_message}>
              {sentence(doc.error_message)}
            </p>
          )}
        </CardContent>
      </Card>

      <AlertDialog
        open={confirmOpen}
        onOpenChange={(open) => {
          if (!remove.isPending) setConfirmOpen(open) // don't allow dismissing mid-request
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this document?</AlertDialogTitle>
            <AlertDialogDescription>
              “{doc.filename}”, its summary and its chat history will be permanently deleted. This
              can’t be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={remove.isPending}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              disabled={remove.isPending}
              onClick={(event) => {
                event.preventDefault() // keep the dialog open until the request finishes
                remove.mutate(doc.id, { onSuccess: () => setConfirmOpen(false) })
              }}
            >
              {remove.isPending && <Loader2 className="animate-spin" />}
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

export function DocumentCardSkeleton() {
  return (
    <Card className="gap-4" aria-hidden>
      <CardContent className="space-y-4">
        <div className="flex items-start gap-3">
          <Skeleton className="size-10 rounded-lg" />
          <div className="flex-1 space-y-2">
            <Skeleton className="h-4 w-3/4" />
            <Skeleton className="h-3 w-1/2" />
          </div>
        </div>
        <Skeleton className="h-5 w-20 rounded-full" />
      </CardContent>
    </Card>
  )
}