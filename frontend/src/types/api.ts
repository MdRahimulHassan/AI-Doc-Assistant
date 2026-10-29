// ---- Envelope (pkg/response) ----
export interface FieldError {
  field: string
  message: string
}

export interface ApiErrorBody {
  code: string
  message: string
  details?: FieldError[] | { status?: string } | unknown
}

export interface ApiEnvelope<T> {
  success: boolean
  data?: T
  error?: ApiErrorBody
}

// ---- Auth ----
export interface User {
  id: string
  email: string
  created_at: string
}

export interface LoginResponse {
  token: string
  token_type: string
  expires_at: string
  user: User
}

// ---- Documents ----
export type DocumentStatus = "processing" | "ready" | "failed"
export type SummaryStatus = "none" | "processing" | "ready" | "failed"

export interface Document {
  id: string
  filename: string
  mime_type: string
  size_bytes: number
  status: DocumentStatus
  error_message?: string
  chunk_count: number
  created_at: string
  summary?: string // not present in list responses
  summary_status: SummaryStatus
  summary_error?: string
  summary_updated_at?: string
}

// ---- Messages ----
export type MessageRole = "user" | "assistant"
export type MessageStatus = "complete" | "partial" | "error"

export interface Message {
  id: number
  role: MessageRole
  content: string
  status: MessageStatus
  sources?: number[] // chunk indexes only, in stored history
  created_at: string
}

export interface MessagePage {
  messages: Message[]
  has_more: boolean
  limit: number
  next_before?: number
}

export interface DocumentList {
  documents: Document[]
  limit: number
  offset: number
}