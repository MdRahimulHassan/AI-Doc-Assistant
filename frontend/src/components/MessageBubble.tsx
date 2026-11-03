import { memo, useState } from "react"
import { AlertCircle, Info, Sparkles } from "lucide-react"
import { ErrorCode } from "@/api"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { CooldownButton } from "@/components/CooldownButton"
import { Markdown } from "@/components/Markdown"
import { prepareAnswerMarkdown, sourcesToShow } from "@/lib/citations"
import { sentence } from "@/lib/text"
import { cn } from "@/lib/utils"
import type { ChatError, ChatMessage, ChatSource } from "@/types/chat"

const NON_RETRYABLE = new Set<string>([
  ErrorCode.DocumentNotReady,
  ErrorCode.NotFound,
  ErrorCode.Validation,
  ErrorCode.Unauthorized,
])

function TypingDots() {
  return (
    <div className="flex items-center gap-1 py-2" role="status" aria-label="Thinking">
      {[0, 1, 2].map((i) => (
        <span
          key={i}
          className="size-1.5 animate-bounce rounded-full bg-muted-foreground/60 motion-reduce:animate-none"
          style={{ animationDelay: `${i * 150}ms` }}
        />
      ))}
    </div>
  )
}

function ErrorNotice({ error, onRetry }: { error: ChatError; onRetry?: () => void }) {
  const canRetry = onRetry && !NON_RETRYABLE.has(error.code)
  return (
    <Alert variant="destructive" role="alert">
      <AlertCircle />
      <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
        <span>{sentence(error.message)}</span>
        {canRetry && (
          <CooldownButton size="sm" variant="outline" cooldown={error.retryAfter} onClick={onRetry}>
            Try again
          </CooldownButton>
        )}
      </AlertDescription>
    </Alert>
  )
}

function SourceList({
  sources,
  cited,
  open,
  onToggle,
}: {
  sources: ChatSource[]
  cited: boolean
  open: number | null
  onToggle: (n: number) => void
}) {
  const active = sources.find((s) => s.number === open)
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">{cited ? "Sources" : "Passages searched"}</span>
        {sources.map((s) => (
          <button
            key={s.number}
            type="button"
            aria-expanded={open === s.number}
            onClick={() => onToggle(s.number)}
            className={cn(
              "rounded-full border px-2 py-0.5 text-xs font-medium transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50",
              open === s.number
                ? "border-primary bg-primary/10 text-primary"
                : "border-border text-muted-foreground hover:bg-accent hover:text-accent-foreground",
            )}
          >
            Source {s.number}
          </button>
        ))}
      </div>
      {active && (
        <div className="space-y-1 rounded-lg border border-border bg-card p-3 text-xs">
          <p className="font-medium">
            Source {active.number} · passage {active.chunkIndex + 1} of the document
          </p>
          <p className="text-muted-foreground">
            {active.preview ?? "A text preview isn’t stored for earlier conversations."}
          </p>
        </div>
      )}
    </div>
  )
}

interface MessageBubbleProps {
  message: ChatMessage
  /** Only passed for the last failed answer. */
  onRetry?: () => void
}

function MessageBubbleBase({ message, onRetry }: MessageBubbleProps) {
  const [openSource, setOpenSource] = useState<number | null>(null)

  if (message.role === "user") {
    return (
      <div className="flex justify-end">
        <div className="max-w-[85%] rounded-2xl rounded-br-md bg-primary px-4 py-2 text-sm break-words whitespace-pre-wrap text-primary-foreground">
          {message.content}
        </div>
      </div>
    )
  }

  const streaming = message.status === "streaming"
  const { sources, cited } = sourcesToShow(message)

  return (
    <div className="flex items-start gap-3">
      <span className="grid size-8 shrink-0 place-items-center rounded-full bg-primary/10 text-primary">
        <Sparkles className="size-4" />
      </span>

      <div className="min-w-0 flex-1 space-y-2">
        {streaming && !message.content ? (
          <TypingDots />
        ) : message.content ? (
          <div className="max-w-[90%] rounded-2xl rounded-tl-md bg-muted px-4 py-3 text-sm">
            <Markdown
              onCitationClick={(n) => {
                if (sources.some((s) => s.number === n)) setOpenSource((cur) => (cur === n ? null : n))
              }}
            >
              {prepareAnswerMarkdown(message.content, streaming)}
            </Markdown>
          </div>
        ) : !message.error ? (
          <p className="text-sm text-muted-foreground italic">
            {message.status === "partial" ? "Stopped before any text was written." : "No answer was generated."}
          </p>
        ) : null}

        {message.status === "partial" && message.content && !message.error && (
          <p className="flex items-center gap-1 text-xs text-muted-foreground">
            <Info className="size-3" /> Stopped early. This answer may be incomplete.
          </p>
        )}

        {message.error && <ErrorNotice error={message.error} onRetry={onRetry} />}

        {!streaming && sources.length > 0 && (
          <SourceList
            sources={sources}
            cited={cited}
            open={openSource}
            onToggle={(n) => setOpenSource((cur) => (cur === n ? null : n))}
          />
        )}
      </div>
    </div>
  )
}

// Older messages never change, so memo keeps them from re-rendering on every streamed token.
export const MessageBubble = memo(MessageBubbleBase)