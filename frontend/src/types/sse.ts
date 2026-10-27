export interface SourcePayload {
  label: string
  chunk_index: number
  preview: string
  truncated?: boolean
}

export interface SourcesEvent { sources: SourcePayload[] }
export interface TokenEvent { text: string }
export interface DoneEvent {
  message_id: number
  status: string
  finish_reason?: string
}
export interface ErrorEvent {
  code: string
  message: string
  retry_after_seconds?: number
  message_id?: number
}