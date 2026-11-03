import { useState, type KeyboardEvent } from "react"
import { ArrowUp, Square } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { config } from "@/lib/config"
import { cn } from "@/lib/utils"

interface ChatComposerProps {
  disabledReason?: string // set when the document can't answer questions
  isStreaming: boolean
  onSend: (question: string) => void
  onStop: () => void
}

export function ChatComposer({ disabledReason, isStreaming, onSend, onStop }: ChatComposerProps) {
  const [value, setValue] = useState("")
  const max = config.maxQuestionChars
  const length = [...value].length // code points, like the server counts characters
  const tooLong = length > max
  const disabled = Boolean(disabledReason)
  const canSend = !disabled && !isStreaming && value.trim().length > 0 && !tooLong

  function submit() {
    if (!canSend) return
    onSend(value)
    setValue("")
  }

  function handleKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    // isComposing: Enter that confirms an IME candidate must not send the message.
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault()
      submit()
    }
  }

  return (
    <div className="space-y-2 border-t border-border p-4">
      <div className="flex items-end gap-2">
        <Textarea
          value={value}
          rows={1}
          disabled={disabled}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder={disabledReason ?? "Ask a question about this document…"}
          aria-label="Your question"
          aria-invalid={tooLong || undefined}
          className="max-h-40 min-h-10 resize-none"
        />
        {isStreaming ? (
          <Button type="button" variant="outline" size="icon" onClick={onStop} aria-label="Stop generating">
            <Square className="fill-current" />
          </Button>
        ) : (
          <Button type="button" size="icon" onClick={submit} disabled={!canSend} aria-label="Send question">
            <ArrowUp />
          </Button>
        )}
      </div>

      <div className="flex items-center justify-between gap-3 text-xs text-muted-foreground">
        <span className="hidden sm:inline">Enter to send · Shift+Enter for a new line</span>
        <span
          className={cn("ml-auto tabular-nums", tooLong && "font-medium text-destructive")}
          aria-live="polite"
        >
          {length > max * 0.8 && `${length}/${max}`}
        </span>
      </div>
    </div>
  )
}