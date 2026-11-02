const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

export const PASSWORD_MIN_CHARS = 8
export const PASSWORD_MAX_BYTES = 72 // bcrypt ignores everything after 72 bytes

export function validateEmail(value: string): string | undefined {
  const email = value.trim()
  if (!email) return "Enter your email address."
  if (email.length > 254 || !EMAIL_PATTERN.test(email)) return "Enter a valid email address."
  return undefined
}

/** For sign-up only. On login we never enforce rules, so old accounts can't get locked out. */
export function validateNewPassword(value: string): string | undefined {
  if ([...value].length < PASSWORD_MIN_CHARS) {
    return `Use at least ${PASSWORD_MIN_CHARS} characters.`
  }
  if (new TextEncoder().encode(value).length > PASSWORD_MAX_BYTES) {
    return `That password is too long. Use ${PASSWORD_MAX_BYTES} bytes or fewer.`
  }
  return undefined
}

export function validateConfirm(password: string, confirm: string): string | undefined {
  return confirm === password ? undefined : "Passwords don’t match."
}

export function hasErrors(errors: Record<string, string | undefined>): boolean {
  return Object.values(errors).some(Boolean)
}