import { useEffect, useState } from "react"

/**
 * Counts down from `seconds` to 0, once per second. The starting value is read
 * when the component mounts, so give the component a `key` that changes per attempt.
 */
export function useCountdown(seconds: number | undefined): number {
  const [remaining, setRemaining] = useState(seconds ?? 0)

  useEffect(() => {
    if (!seconds) return
    const endsAt = Date.now() + seconds * 1000
    const id = window.setInterval(() => {
      const left = Math.max(0, Math.ceil((endsAt - Date.now()) / 1000))
      setRemaining(left)
      if (left === 0) window.clearInterval(id)
    }, 1000)
    return () => window.clearInterval(id)
  }, [seconds])

  return seconds ? remaining : 0
}