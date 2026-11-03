export interface SSEFrame {
  event: string
  data: string
}

function parseFrame(raw: string): SSEFrame | null {
  let event = "message"
  const data: string[] = []

  for (const line of raw.split("\n")) {
    if (line === "" || line.startsWith(":")) continue // blank, or a ": ping" heartbeat comment

    const colon = line.indexOf(":")
    const field = colon === -1 ? line : line.slice(0, colon)
    let value = colon === -1 ? "" : line.slice(colon + 1)
    if (value.startsWith(" ")) value = value.slice(1) // the spec strips ONE leading space

    if (field === "event") event = value
    else if (field === "data") data.push(value)
  }

  // A frame with no data lines (a heartbeat) carries nothing.
  return data.length > 0 ? { event, data: data.join("\n") } : null
}

/**
 * Reads a text/event-stream body and yields one frame per blank-line-terminated
 * block. Network chunks do NOT line up with frames: a chunk can end in the
 * middle of a frame (or of a multi-byte character), so we keep a buffer and
 * only cut it at a complete "\n\n".
 */
export async function* readSSE(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<SSEFrame, void, undefined> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""

  try {
    while (true) {
      const { value, done } = await reader.read()
      if (done) break

      // {stream: true} keeps a half-received UTF-8 character for the next chunk.
      buffer = (buffer + decoder.decode(value, { stream: true })).replace(/\r\n/g, "\n")

      let boundary = buffer.indexOf("\n\n")
      while (boundary !== -1) {
        const frame = parseFrame(buffer.slice(0, boundary))
        buffer = buffer.slice(boundary + 2)
        if (frame) yield frame
        boundary = buffer.indexOf("\n\n")
      }
    }
    // Whatever is left in the buffer never got its blank line: an incomplete frame, dropped.
  } finally {
    reader.cancel().catch(() => {}) // closes the connection if the consumer stopped early
  }
}