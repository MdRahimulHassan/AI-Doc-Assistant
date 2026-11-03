import { useLayoutEffect, useRef, useState } from "react"
import { ArrowDown, Loader2, MessageSquareText } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { ChatComposer } from "@/components/ChatComposer"
import { MessageBubble } from "@/components/MessageBubble"
import { useChat } from "@/hooks/useChat"
import type { Document } from "@/types/api"

const SUGGESTIONS = [
  "What is this document about?",
  "Summarize the key points.",
  "Which dates or deadlines are mentioned?",
]

export function ChatWindow({ doc }: { doc: Document }) {
  const chat = useChat(doc.id)
  const { messages, history } = chat

  const scrollRef = useRef<HTMLDivElement>(null)
  const stickToBottom = useRef(true) // false once the user scrolls up
  const anchor = useRef<{ height: number; top: number } | null>(null) // for "load earlier"
  const [showJump, setShowJump] = useState(false)

  const ready = doc.status === "ready"
  const disabledReason = ready
    ? undefined
    : doc.status === "failed"
      ? "This document couldn’t be processed, so it can’t answer questions."
      : "This document is still being processed. Questions unlock when it’s ready."

  const lastAssistantKey = [...messages].reverse().find((m) => m.role === "assistant")?.key

  // 1) After older messages are inserted above, put the viewport back where it was.
  useLayoutEffect(() => {
    const el = scrollRef.current
    const saved = anchor.current
    if (!el || !saved) return
    el.scrollTop = saved.top + (el.scrollHeight - saved.height)
    anchor.current = null
  }, [history.count])

  // 2) Follow new text, unless the user scrolled away from the bottom.
  useLayoutEffect(() => {
    const el = scrollRef.current
    if (el && stickToBottom.current) el.scrollTop = el.scrollHeight
  }, [messages])

  function handleScroll() {
    const el = scrollRef.current
    if (!el) return
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight
    stickToBottom.current = distance < 80
    setShowJump(distance > 240)
  }

  function handleSend(question: string) {
    stickToBottom.current = true
    void chat.send(question)
  }

  function loadEarlier() {
    const el = scrollRef.current
    if (el) anchor.current = { height: el.scrollHeight, top: el.scrollTop }
    void history.loadEarlier()
  }

  function jumpToLatest() {
    const el = scrollRef.current
    if (!el) return
    stickToBottom.current = true
    el.scrollTo({ top: el.scrollHeight, behavior: "smooth" })
  }

  return (
    <Card className="h-full min-h-0 gap-0 py-0">
      <div className="border-b border-border px-6 py-4">
        <h2 className="text-sm font-semibold">Chat</h2>
      </div>

      <div className="relative min-h-0 flex-1">
        <div
          ref={scrollRef}
          onScroll={handleScroll}
          role="log"
          aria-label="Conversation"
          aria-live="off"
          aria-busy={chat.isStreaming}
          className="h-full overflow-y-auto px-4 py-6"
        >
          {history.isPending && (
            <div className="space-y-6" aria-busy="true">
              <Skeleton className="h-10 w-2/5 ml-auto rounded-2xl" />
              <Skeleton className="h-20 w-4/5 rounded-2xl" />
            </div>
          )}

          {history.isError && messages.length === 0 && (
            <div className="space-y-3 text-center text-sm text-muted-foreground">
              <p>Couldn’t load the earlier conversation.</p>
              <Button variant="outline" size="sm" onClick={() => void history.reload()}>
                Try again
              </Button>
            </div>
          )}

          {history.hasMore && (
            <div className="mb-6 flex justify-center">
              <Button variant="ghost" size="sm" onClick={loadEarlier} disabled={history.isLoadingEarlier}>
                {history.isLoadingEarlier && <Loader2 className="animate-spin" />}
                {history.loadEarlierFailed ? "Couldn’t load. Try again" : "Load earlier messages"}
              </Button>
            </div>
          )}

          {!history.isPending && messages.length === 0 && !history.isError && (
            <div className="flex min-h-full flex-col items-center justify-center gap-6 text-center">
              <span className="grid size-12 place-items-center rounded-full bg-primary/10 text-primary">
                <MessageSquareText className="size-6" />
              </span>
              <div className="space-y-1">
                <h3 className="font-medium">Ask about this document</h3>
                <p className="max-w-xs text-sm text-muted-foreground">
                  Answers come from the document itself and cite the passages they use.
                </p>
              </div>
              {ready && (
                <div className="flex flex-wrap justify-center gap-2">
                  {SUGGESTIONS.map((suggestion) => (
                    <Button key={suggestion} variant="outline" size="sm" onClick={() => handleSend(suggestion)}>
                      {suggestion}
                    </Button>
                  ))}
                </div>
              )}
            </div>
          )}

          <ul className="space-y-6">
            {messages.map((message) => (
              <li key={message.key}>
                <MessageBubble
                  message={message}
                  onRetry={
                    message.key === lastAssistantKey && message.question
                      ? () => chat.retry(message.key)
                      : undefined
                  }
                />
              </li>
            ))}
          </ul>
        </div>

        {showJump && (
          <Button
            size="sm"
            variant="secondary"
            className="absolute bottom-3 left-1/2 -translate-x-1/2 shadow-md"
            onClick={jumpToLatest}
          >
            <ArrowDown /> Latest
          </Button>
        )}
      </div>

      <ChatComposer
        disabledReason={disabledReason}
        isStreaming={chat.isStreaming}
        onSend={handleSend}
        onStop={chat.stop}
      />
    </Card>
  )
}