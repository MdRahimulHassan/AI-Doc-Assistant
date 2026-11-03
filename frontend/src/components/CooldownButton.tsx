import type { ComponentProps } from "react"
import { Button } from "@/components/ui/button"
import { useCountdown } from "@/hooks/useCountdown"

interface CooldownButtonProps extends ComponentProps<typeof Button> {
  /** Seconds to stay disabled after mounting (from Retry-After). Remount with a new key to restart. */
  cooldown?: number
}

export function CooldownButton({ cooldown, disabled, children, ...props }: CooldownButtonProps) {
  const remaining = useCountdown(cooldown)
  return (
    <Button disabled={disabled || remaining > 0} {...props}>
      {children}
      {remaining > 0 && <span className="tabular-nums">({remaining}s)</span>}
    </Button>
  )
}