import { config } from "@/lib/config"
import { readSSE, type SSEFrame } from "@/lib/sse"
import type {
  DoneEvent,
  ErrorEvent as StreamErrorEvent,
  SourcesEvent,
  TokenEvent,
} from "@/types/sse"
import {
  ApiError,
  ErrorCode,
  canceledError,
  errorFromResponse,
  networkError,
  unknownError,
} from "./errors"
import { getToken, handleRejectedToken } from "./session"

export type AskEvent =
  | { type: "sources"; data: SourcesEvent }
  | { type: "token"; data: TokenEvent }
  | { type: "done"; data: DoneEvent }
  | { type: "error"; data: StreamErrorEvent }

function toAskEvent(frame: SSEFrame): AskEvent | null {
  try {
    const data = JSON.parse(frame.data)
    switch (frame.event) {
      case "sources":
        return { type: "sources", data }
      case "token":
        return { type: "token", data }
      case "done":
        return { type: "done", data }
      case "error":
        return { type: "error", data }
    }
  } catch {
    /* malformed frame: skip it */
  }
  return null // unknown event names are ignored, so the server can add new ones safely
}

const isAbort = (error: unknown) => error instanceof DOMException && error.name === "AbortError"

const interruptedError = () =>
  new ApiError({
    code: ErrorCode.Network,
    status: 0,
    message: "The connection was interrupted before the answer finished.",
  })

/**
 * POSTs a question and reports the SSE events as they arrive. Resolves when the
 * stream ends. Rejects with an ApiError for everything else, including a user
 * abort (code CANCELED). Axios can't do this: it doesn't expose the body while
 * it is still arriving.
 */
export async function streamAnswer(
  documentId: string,
  question: string,
  { signal, onEvent }: { signal?: AbortSignal; onEvent: (event: AskEvent) => void },
): Promise<void> {
  const token = getToken()

  let response: Response
  try {
    response = await fetch(`${config.apiUrl}/api/documents/${documentId}/ask`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "text/event-stream",
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({ question }),
      signal,
    })
  } catch (error) {
    throw isAbort(error) ? canceledError() : networkError()
  }

  // Not 200 means a normal JSON error: parse the envelope exactly like axios does.
  if (!response.ok) {
    let body: unknown = null
    try {
      body = await response.json()
    } catch {
      /* not JSON (a proxy page, say) */
    }
    const apiError = errorFromResponse(response.status, body, {
      retryAfter: response.headers.get("retry-after"),
      requestId: response.headers.get("x-request-id"),
    })
    if (apiError.status === 401 && apiError.code === ErrorCode.Unauthorized) {
      handleRejectedToken(token ? `Bearer ${token}` : null)
    }
    throw apiError
  }

  if (!response.body) throw unknownError()

  try {
    for await (const frame of readSSE(response.body)) {
      const event = toAskEvent(frame)
      if (event) onEvent(event)
    }
  } catch (error) {
    throw isAbort(error) ? canceledError() : interruptedError()
  }
}