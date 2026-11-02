import { useState } from "react"
import { useDropzone } from "react-dropzone"
import { AlertCircle, UploadCloud, X } from "lucide-react"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Progress } from "@/components/ui/progress"
import { useUploadDocument } from "@/hooks/useDocuments"
import { config } from "@/lib/config"
import {
  ACCEPTED_TYPES,
  MAX_UPLOAD_LABEL,
  describeRejection,
  describeUploadError,
} from "@/lib/files"
import { formatFileSize } from "@/lib/format"
import { cn } from "@/lib/utils"

export function UploadDropzone() {
  const { upload, reset, cancel, isUploading, progress, file, error } = useUploadDocument()
  const [rejection, setRejection] = useState<string | null>(null)

  const { getRootProps, getInputProps, isDragActive, isDragReject } = useDropzone({
    accept: ACCEPTED_TYPES,
    maxSize: config.maxUploadBytes,
    minSize: 1, // rejects empty files with "file-too-small"
    multiple: false,
    disabled: isUploading,
    onDropAccepted: ([accepted]) => {
      setRejection(null)
      reset()
      upload(accepted)
    },
    onDropRejected: ([first]) => {
      reset()
      setRejection(describeRejection(first))
    },
  })

  // A client-side rejection wins; otherwise show the server's verdict (null for a cancel).
  const message = rejection ?? (error ? describeUploadError(error) : null)

  return (
    <div className="space-y-3">
      <div
        {...getRootProps({
          "aria-label": "Upload a document",
          className: cn(
            "flex flex-col items-center justify-center gap-4 rounded-xl border-2 border-dashed border-border bg-card px-6 py-10 text-center transition-colors",
            "outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50",
            isUploading
              ? "cursor-default"
              : "cursor-pointer hover:border-primary/50 hover:bg-accent/40",
            isDragActive && !isDragReject && "border-primary bg-accent",
            isDragReject && "border-destructive bg-destructive/5",
          ),
        })}
      >
        <input {...getInputProps()} />

        {isUploading ? (
          <div role="status" className="flex w-full max-w-sm flex-col items-center gap-4">
            <div className="w-full space-y-2">
              <div className="flex items-center justify-between gap-3 text-sm">
                <span className="truncate font-medium">{file?.name}</span>
                <span className="shrink-0 text-muted-foreground">
                  {progress < 100 ? `${progress}%` : "Finishing…"}
                </span>
              </div>
              <Progress value={progress} />
              {file && <p className="text-left text-xs text-muted-foreground">{formatFileSize(file.size)}</p>}
            </div>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={(event) => {
                event.stopPropagation()
                cancel()
              }}
            >
              <X /> Cancel
            </Button>
          </div>
        ) : (
          <>
            <span className="grid size-12 place-items-center rounded-full bg-primary/10 text-primary">
              <UploadCloud className="size-6" />
            </span>
            <div className="space-y-1">
              <p className="text-sm font-medium">
                {isDragReject ? (
                  "This file type isn’t supported"
                ) : isDragActive ? (
                  "Drop to upload"
                ) : (
                  <>
                    Drag a file here, or <span className="text-primary">browse</span>
                  </>
                )}
              </p>
              <p className="text-xs text-muted-foreground">
                PDF, DOCX or TXT · up to {MAX_UPLOAD_LABEL}
              </p>
            </div>
          </>
        )}
      </div>

      {message && (
        <Alert variant="destructive" role="alert">
          <AlertCircle />
          <AlertDescription>{message}</AlertDescription>
        </Alert>
      )}
    </div>
  )
}