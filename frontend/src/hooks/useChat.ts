/*import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { useInfiniteQuery } from "@tanstack/react-query"
import { ApiError, ErrorCode, assistantApi, streamAnswer } from "@/api"
import type { Message } from "@/types/api"
import type { SourcePayload } from "@/types/sse"
import type { ChatError, ChatMessage, ChatSource } from "@/types/chat"

const PAGE_SIZE = 30

function toChatSources(sources: SourcePayload[]): ChatSource[] {
  return sources.map((s, i) => ({
    number: Number(s.label.replace(/\D/g, "")) || i + 1, // "P2" -> 2
    chunkIndex: s.chunk_index,
    preview: s.preview,
  }))
}

// Stored sources are chunk indexes in passage order: P1 = sources[0], P2 = sources[1], ...
function fromHistory(m: Message): ChatMessage {
  return {
    key: `msg-${m.id}`,
    id: m.id,
    role: m.role,
    content: m.content,
    status: m.status,
    sources: (m.sources ?? []).map((chunkIndex, i) => ({ number: i + 1, chunkIndex })),
  }
}

function toChatError(error: ApiError | null): ChatError {
  if (!error) return { code: ErrorCode.Unknown, message: "Something went wrong. Please try again." }
  const message = error.is(ErrorCode.Validation)
    ? (error.fieldErrors.question ?? error.message)
    : error.message
  return {
    code: error.code,
    message,
    // A second tab is still answering: it normally finishes within seconds.
    retryAfter: error.retryAfter ?? (error.is(ErrorCode.AnswerInProgress) ? 3 : undefined),
  }
}

export function useChat(documentId: string) {
  const [live, setLive] = useState<ChatMessage[]>([]) // messages created in THIS session
  const [isStreaming, setIsStreaming] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const counterRef = useRef(0)

  // History: newest page first, then older pages on demand. Never refetched
  // mid-session (that would duplicate what `live` already shows); gcTime 0
  // makes every visit start from a fresh server copy.
  const history = useInfiniteQuery({
    queryKey: ["messages", documentId],
    queryFn: ({ pageParam, signal }) =>
      assistantApi.messages(documentId, { limit: PAGE_SIZE, before: pageParam }, signal),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => (last.has_more ? last.next_before : undefined),
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })

  // pages[0] is the newest page; each page is oldest-first inside. Reverse the pages to read top-down.
  const older = useMemo(
    () => [...(history.data?.pages ?? [])].reverse().flatMap((p) => p.messages.map(fromHistory)),
    [history.data],
  )
  const messages = useMemo(() => [...older, ...live], [older, live])

  // Leaving the page cancels an answer in flight (the server saves what it had).
  useEffect(() => () => abortRef.current?.abort(), [])

  const patch = useCallback((key: string, change: (m: ChatMessage) => ChatMessage) => {
    setLive((list) => list.map((m) => (m.key === key ? change(m) : m)))
  }, [])

  const send = useCallback(
    async (raw: string) => {
      const question = raw.trim()
      if (!question || abortRef.current) return // empty, or an answer is already streaming

      const n = ++counterRef.current
      const botKey = `live-a-${n}`
      setLive((list) => [
        ...list,
        { key: `live-u-${n}`, role: "user", content: question, status: "complete", sources: [] },
        { key: botKey, role: "assistant", content: "", status: "streaming", sources: [], question },
      ])

      const controller = new AbortController()
      abortRef.current = controller
      setIsStreaming(true)

      // Tokens arrive faster than the screen refreshes: collect them, apply once per frame.
      let buffered = ""
      let frame: number | null = null
      const flush = () => {
        if (frame !== null) {
          cancelAnimationFrame(frame)
          frame = null
        }
        if (!buffered) return
        const text = buffered
        buffered = ""
        patch(botKey, (m) => ({ ...m, content: m.content + text }))
      }

      const outcome = { finished: false } // true once done/error arrived

      try {
        await streamAnswer(documentId, question, {
          signal: controller.signal,
          onEvent: (event) => {
            switch (event.type) {
              case "sources":
                patch(botKey, (m) => ({ ...m, sources: toChatSources(event.data.sources) }))
                break
              case "token":
                buffered += event.data.text
                if (frame === null) frame = requestAnimationFrame(flush)
                break
              case "done":
                flush()
                outcome.finished = true
                patch(botKey, (m) => ({
                  ...m,
                  id: event.data.message_id,
                  status: event.data.status === "partial" ? "partial" : "complete",
                }))
                break
              case "error":
                flush()
                outcome.finished = true
                patch(botKey, (m) => ({
                  ...m,
                  id: event.data.message_id || m.id,
                  status: m.content ? "partial" : "error",
                  error: {
                    code: event.data.code,
                    message: event.data.message,
                    retryAfter: event.data.retry_after_seconds,
                  },
                }))
                break
            }
          },
        })

        if (!outcome.finished) {
          // The stream closed without a done or error event.
          flush()
          patch(botKey, (m) => ({
            ...m,
            status: m.content ? "partial" : "error",
            error: {
              code: ErrorCode.Network,
              message: "The connection closed before the answer finished.",
            },
          }))
        }
      } catch (error) {
        flush()
        const apiError = error instanceof ApiError ? error : null
        if (apiError?.is(ErrorCode.Canceled)) {
          patch(botKey, (m) => ({ ...m, status: "partial" })) // stopped on purpose
        } else {
          patch(botKey, (m) => ({
            ...m,
            status: m.content ? "partial" : "error",
            error: toChatError(apiError),
          }))
        }
      } finally {
        abortRef.current = null
        setIsStreaming(false)
      }
    },
    [documentId, patch],
  )

  const stop = useCallback(() => abortRef.current?.abort(), [])

  // "Try again" on the last failed answer: drop the failed pair, ask again.
  const retry = useCallback(
    (botKey: string) => {
      const index = live.findIndex((m) => m.key === botKey)
      const question = live[index]?.question
      if (index < 1 || !question) return
      setLive((list) => list.filter((_, i) => i !== index && i !== index - 1))
      void send(question)
    },
    [live, send],
  )

  return {
    messages,
    isStreaming,
    send,
    stop,
    retry,
    history: {
      isPending: history.isPending,
      isError: history.isError,
      hasMore: history.hasNextPage,
      isLoadingEarlier: history.isFetchingNextPage,
      loadEarlierFailed: history.isFetchNextPageError,
      count: older.length,
      loadEarlier: () => history.fetchNextPage(),
      reload: () => history.refetch(),
    },
  }
}*/

import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { useInfiniteQuery } from "@tanstack/react-query"
import { ApiError, ErrorCode, assistantApi, streamAnswer } from "@/api"
import type { Message } from "@/types/api"
import type { SourcePayload } from "@/types/sse"
import type { ChatError, ChatMessage, ChatSource } from "@/types/chat"

const PAGE_SIZE = 30

function toChatSources(sources: SourcePayload[]): ChatSource[] {
  return sources.map((s, i) => ({
    number: Number(s.label.replace(/\D/g, "")) || i + 1, // "P2" -> 2
    chunkIndex: s.chunk_index,
    preview: s.preview,
  }))
}

// Stored sources are chunk indexes in passage order: P1 = sources[0], P2 = sources[1], ...
function fromHistory(m: Message): ChatMessage {
  return {
    key: `msg-${m.id}`,
    id: m.id,
    role: m.role,
    content: m.content,
    status: m.status,
    sources: (m.sources ?? []).map((chunkIndex, i) => ({ number: i + 1, chunkIndex })),
  }
}

function toChatError(error: ApiError | null): ChatError {
  if (!error) return { code: ErrorCode.Unknown, message: "Something went wrong. Please try again." }
  const message = error.is(ErrorCode.Validation)
    ? (error.fieldErrors.question ?? error.message)
    : error.message
  return {
    code: error.code,
    message,
    // A second tab is still answering: it normally finishes within seconds.
    retryAfter: error.retryAfter ?? (error.is(ErrorCode.AnswerInProgress) ? 3 : undefined),
  }
}

export function useChat(documentId: string) {
  const [live, setLive] = useState<ChatMessage[]>([]) // messages created in THIS session
  const [isStreaming, setIsStreaming] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const counterRef = useRef(0)

  // History: newest page first, then older pages on demand. Never refetched
  // mid-session (that would duplicate what `live` already shows); gcTime 0
  // makes every visit start from a fresh server copy.
  const history = useInfiniteQuery({
    queryKey: ["messages", documentId],
    queryFn: ({ pageParam, signal }) =>
      assistantApi.messages(documentId, { limit: PAGE_SIZE, before: pageParam }, signal),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => (last.has_more ? last.next_before : undefined),
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })

  // pages[0] is the newest page; each page is oldest-first inside. Reverse the pages to read top-down.
  const older = useMemo(
    () => [...(history.data?.pages ?? [])].reverse().flatMap((p) => p.messages.map(fromHistory)),
    [history.data],
  )
  const messages = useMemo(() => [...older, ...live], [older, live])

  // Leaving the page cancels an answer in flight (the server saves what it had).
  useEffect(() => () => abortRef.current?.abort(), [])

  const patch = useCallback((key: string, change: (m: ChatMessage) => ChatMessage) => {
    setLive((list) => list.map((m) => (m.key === key ? change(m) : m)))
  }, [])

  const send = useCallback(
    async (raw: string) => {
      const question = raw.trim()
      if (!question || abortRef.current) return // empty, or an answer is already streaming

      const n = ++counterRef.current
      const botKey = `live-a-${n}`
      setLive((list) => [
        ...list,
        { key: `live-u-${n}`, role: "user", content: question, status: "complete", sources: [] },
        { key: botKey, role: "assistant", content: "", status: "streaming", sources: [], question },
      ])

      const controller = new AbortController()
      abortRef.current = controller
      setIsStreaming(true)

      // Tokens arrive faster than the screen refreshes: collect them, apply once per frame.
      let buffered = ""
      let frame: number | null = null
      const flush = () => {
        if (frame !== null) {
          cancelAnimationFrame(frame)
          frame = null
        }
        if (!buffered) return
        const text = buffered
        buffered = ""
        patch(botKey, (m) => ({ ...m, content: m.content + text }))
      }

      const outcome = { finished: false } // true once done/error arrived

      try {
        await streamAnswer(documentId, question, {
          signal: controller.signal,
          onEvent: (event: Parameters<NonNullable<Parameters<typeof streamAnswer>[2]["onEvent"]>>[0]) => {
            switch (event.type) {
              case "sources":
                patch(botKey, (m) => ({ ...m, sources: toChatSources(event.data.sources) }))
                break
              case "token":
                buffered += event.data.text
                if (frame === null) frame = requestAnimationFrame(flush)
                break
              case "done":
                flush()
                outcome.finished = true
                patch(botKey, (m) => ({
                  ...m,
                  id: event.data.message_id,
                  status: event.data.status === "partial" ? "partial" : "complete",
                }))
                break
              case "error":
                flush()
                outcome.finished = true
                patch(botKey, (m) => ({
                  ...m,
                  id: event.data.message_id || m.id,
                  status: m.content ? "partial" : "error",
                  error: {
                    code: event.data.code,
                    message: event.data.message,
                    retryAfter: event.data.retry_after_seconds,
                  },
                }))
                break
            }
          },
        })

        if (!outcome.finished) {
          // The stream closed without a done or error event.
          flush()
          patch(botKey, (m) => ({
            ...m,
            status: m.content ? "partial" : "error",
            error: {
              code: ErrorCode.Network,
              message: "The connection closed before the answer finished.",
            },
          }))
        }
      } catch (error) {
        flush()
        const apiError = error instanceof ApiError ? error : null
        if (apiError?.is(ErrorCode.Canceled)) {
          patch(botKey, (m) => ({ ...m, status: "partial" })) // stopped on purpose
        } else {
          patch(botKey, (m) => ({
            ...m,
            status: m.content ? "partial" : "error",
            error: toChatError(apiError),
          }))
        }
      } finally {
        abortRef.current = null
        setIsStreaming(false)
      }
    },
    [documentId, patch],
  )

  const stop = useCallback(() => abortRef.current?.abort(), [])

  // "Try again" on the last failed answer: drop the failed pair, ask again.
  const retry = useCallback(
    (botKey: string) => {
      const index = live.findIndex((m) => m.key === botKey)
      const question = live[index]?.question
      if (index < 1 || !question) return
      setLive((list) => list.filter((_, i) => i !== index && i !== index - 1))
      void send(question)
    },
    [live, send],
  )

  return {
    messages,
    isStreaming,
    send,
    stop,
    retry,
    history: {
      isPending: history.isPending,
      isError: history.isError,
      hasMore: history.hasNextPage,
      isLoadingEarlier: history.isFetchingNextPage,
      loadEarlierFailed: history.isFetchNextPageError,
      count: older.length,
      loadEarlier: () => history.fetchNextPage(),
      reload: () => history.refetch(),
    },
  }
}