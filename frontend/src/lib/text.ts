/** "the file is too large" -> "The file is too large." */
export function sentence(message: string): string {
  const text = message.trim()
  if (!text) return text
  const capitalized = text.charAt(0).toUpperCase() + text.slice(1)
  return /[.!?]$/.test(capitalized) ? capitalized : `${capitalized}.`
}