import { useState, type ComponentProps } from "react"
import { Eye, EyeOff } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { cn } from "@/lib/utils"

interface TextFieldProps extends Omit<ComponentProps<"input">, "id"> {
  id: string
  label: string
  error?: string
  hint?: string
}

export function TextField({ id, label, error, hint, type = "text", className, ...props }: TextFieldProps) {
  const [revealed, setRevealed] = useState(false)
  const isPassword = type === "password"
  const errorId = `${id}-error`
  const hintId = `${id}-hint`

  return (
    <div className="space-y-2">
      <Label htmlFor={id}>{label}</Label>
      <div className="relative">
        <Input
          id={id}
          type={isPassword && revealed ? "text" : type}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? errorId : hint ? hintId : undefined}
          className={cn(isPassword && "pr-10", className)}
          {...props}
        />
        {isPassword && (
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="absolute top-0 right-0 size-9 text-muted-foreground"
            onClick={() => setRevealed((v) => !v)}
            aria-label={revealed ? "Hide password" : "Show password"}
          >
            {revealed ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
          </Button>
        )}
      </div>
      {error ? (
        <p id={errorId} className="text-xs text-destructive">
          {error}
        </p>
      ) : hint ? (
        <p id={hintId} className="text-xs text-muted-foreground">
          {hint}
        </p>
      ) : null}
    </div>
  )
}