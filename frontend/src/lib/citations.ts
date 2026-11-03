import type { ChatMessage, ChatSource } from "@/types/chat"

// Mirrors the backend: "[P1]" or "[P1, P3]".
const CITATION_GROUP = /\[\s*P\d+(?:\s*[,;]\s*P\d+)*\s*\]/g
const CITATION_NUMBER = /P(\d+)/g

/** Passage numbers an answer cites, each once, in order of first appearance. */
export function citedNumbers(text: string): number[] {
  const seen = new Set<number>()
  for (const group of text.matchAll(CITATION_GROUP)) {
    for (const match of group[0].matchAll(CITATION_NUMBER)) seen.add(Number(match[1]))
  }
  return [...seen]
}

/**
 * Rewrites "[P1, P3]" as markdown links "[1](cite:1) [3](cite:3)", which the
 * Markdown component renders as clickable chips. While streaming, an unfinished
 * marker at the very end ("[P") is hidden so it doesn't flicker.
 */
export function prepareAnswerMarkdown(text: string, streaming: boolean): string {
  const visible = streaming ? text.replace(/\[(?:\s|P|\d|,|;)*$/, "") : text
  return visible.replace(CITATION_GROUP, (group) =>
    Array.from(group.matchAll(CITATION_NUMBER), (m) => `[${m[1]}](cite:${m[1]})`).join(" "),
  )
}

/**
 * Which sources to list under an answer: the ones it cites. If it cites none
 * (rare), fall back to everything that was searched, labelled honestly.
 */
export function sourcesToShow(message: ChatMessage): { sources: ChatSource[]; cited: boolean } {
  const cited = new Set(citedNumbers(message.content))
  const used = message.sources.filter((s) => cited.has(s.number))
  return used.length > 0 ? { sources: used, cited: true } : { sources: message.sources, cited: false }
}