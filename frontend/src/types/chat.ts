export type ChatStatus = "streaming" | "complete" | "partial" | "error"

export interface ChatSource {
  number: number // the "P" number an answer cites: [P1] -> 1
  chunkIndex: number // 0-based position of the passage in the document
  preview?: string // only available for answers streamed in this session
}

export interface ChatError {
  code: string
  message: string
  retryAfter?: number
}

export interface ChatMessage {
  key: string // stable React key
  id?: number // server id, once known
  role: "user" | "assistant"
  content: string
  status: ChatStatus
  sources: ChatSource[]
  error?: ChatError
  question?: string // assistant messages from this session: what was asked, for "Try again"
}