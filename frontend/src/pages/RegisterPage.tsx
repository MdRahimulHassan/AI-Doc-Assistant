import { useState, type FormEvent } from "react"
import { Link, useNavigate } from "react-router-dom"
import { AlertCircle, Loader2 } from "lucide-react"
import { toast } from "sonner"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { AuthLayout } from "@/components/AuthLayout"
import { TextField } from "@/components/TextField"
import { useAuth } from "@/hooks/useAuth"
import { mapAuthError, type AuthField, type FieldErrors } from "@/lib/auth-errors"
import {
  PASSWORD_MIN_CHARS,
  hasErrors,
  validateConfirm,
  validateEmail,
  validateNewPassword,
} from "@/lib/validation"

export default function RegisterPage() {
  const { register } = useAuth()
  const navigate = useNavigate()
  const [email, setEmail] = useState("")
  const [password, setPassword] = useState("")
  const [confirm, setConfirm] = useState("")
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
      password: validateNewPassword(password),
      confirmPassword: validateConfirm(password, confirm),
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
      const { signedIn } = await register({ email: email.trim(), password })
      if (!signedIn) {
        // The account exists but the automatic sign-in failed (rare). Hand over to the login page.
        toast.success("Account created. Please sign in.")
        navigate("/login", { replace: true })
      }
      // If signedIn, GuestRoute redirects to the dashboard.
    } catch (error) {
      const mapped = mapAuthError(error)
      setErrors(mapped.fields)
      setFormError(mapped.form)
      setSubmitting(false)
    }
  }

  return (
    <AuthLayout
      title="Create your account"
      subtitle="Start asking questions about your documents."
      footer={
        <>
          Already have an account?{" "}
          <Link to="/login" className="font-medium text-primary underline-offset-4 hover:underline">
            Sign in
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
          autoComplete="new-password"
          hint={`At least ${PASSWORD_MIN_CHARS} characters.`}
          value={password}
          error={errors.password}
          onChange={(e) => {
            setPassword(e.target.value)
            clearError("password")
          }}
        />
        <TextField
          id="confirm-password"
          label="Confirm password"
          type="password"
          autoComplete="new-password"
          value={confirm}
          error={errors.confirmPassword}
          onChange={(e) => {
            setConfirm(e.target.value)
            clearError("confirmPassword")
          }}
        />

        <Button type="submit" className="w-full" disabled={submitting}>
          {submitting && <Loader2 className="animate-spin" />}
          Create account
        </Button>
      </form>
    </AuthLayout>
  )
}