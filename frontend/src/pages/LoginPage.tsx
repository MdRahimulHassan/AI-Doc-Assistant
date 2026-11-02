import { useState, type FormEvent } from "react"
import { Link } from "react-router-dom"
import { AlertCircle, Loader2 } from "lucide-react"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { AuthLayout } from "@/components/AuthLayout"
import { TextField } from "@/components/TextField"
import { useAuth } from "@/hooks/useAuth"
import { mapAuthError, type AuthField, type FieldErrors } from "@/lib/auth-errors"
import { hasErrors, validateEmail } from "@/lib/validation"

export default function LoginPage() {
  const { login } = useAuth()
  const [email, setEmail] = useState("")
  const [password, setPassword] = useState("")
  const [errors, setErrors] = useState<FieldErrors>({})
  const [formError, setFormError] = useState<string>()
  const [submitting, setSubmitting] = useState(false)

  function clearError(field: AuthField) {
    setErrors((current) => ({ ...current, [field]: undefined }))
    setFormError(undefined)
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitting) return

    const clientErrors: FieldErrors = {
      email: validateEmail(email),
      password: password ? undefined : "Enter your password.",
    }
    if (hasErrors(clientErrors)) {
      setErrors(clientErrors)
      setFormError(undefined)
      return
    }

    setErrors({})
    setFormError(undefined)
    setSubmitting(true)
    try {
      await login({ email: email.trim(), password })
      // Success: GuestRoute notices the user and redirects. Nothing to do here.
    } catch (error) {
      const mapped = mapAuthError(error)
      setErrors(mapped.fields)
      setFormError(mapped.form)
      setSubmitting(false)
    }
  }

  return (
    <AuthLayout
      title="Welcome back"
      subtitle="Sign in to your account to continue."
      footer={
        <>
          Don’t have an account?{" "}
          <Link to="/register" className="font-medium text-primary underline-offset-4 hover:underline">
            Create one
          </Link>
        </>
      }
    >
      <form onSubmit={handleSubmit} noValidate className="space-y-4">
        {formError && (
          <Alert variant="destructive" role="alert">
            <AlertCircle />
            <AlertDescription>{formError}</AlertDescription>
          </Alert>
        )}

        <TextField
          id="email"
          label="Email"
          type="email"
          autoComplete="email"
          placeholder="you@example.com"
          autoFocus
          value={email}
          error={errors.email}
          onChange={(e) => {
            setEmail(e.target.value)
            clearError("email")
          }}
        />
        <TextField
          id="password"
          label="Password"
          type="password"
          autoComplete="current-password"
          value={password}
          error={errors.password}
          onChange={(e) => {
            setPassword(e.target.value)
            clearError("password")
          }}
        />

        <Button type="submit" className="w-full" disabled={submitting}>
          {submitting && <Loader2 className="animate-spin" />}
          Sign in
        </Button>
      </form>
    </AuthLayout>
  )
}