import type { ReactNode } from "react"
import { FileUp, Quote, ShieldCheck } from "lucide-react"
import { Logo } from "@/components/Logo"
import { ThemeToggle } from "@/components/ThemeToggle"

const FEATURES = [
  {
    icon: FileUp,
    title: "Bring your own documents",
    text: "Upload PDF, Word or plain-text files.",
  },
  {
    icon: Quote,
    title: "Answers with sources",
    text: "Replies are grounded in your document and point back to the passages they came from.",
  },
  {
    icon: ShieldCheck,
    title: "Private by default",
    text: "Every document is visible only to your account.",
  },
]

interface AuthLayoutProps {
  title: string
  subtitle: string
  footer: ReactNode
  children: ReactNode
}

export function AuthLayout({ title, subtitle, footer, children }: AuthLayoutProps) {
  return (
    <div className="grid min-h-screen bg-background text-foreground lg:grid-cols-2">
      <aside className="relative hidden overflow-hidden bg-linear-to-br from-primary to-[color-mix(in_oklab,var(--primary),black_40%)] p-12 text-primary-foreground lg:flex lg:flex-col lg:justify-between">
        <div
          aria-hidden
          className="pointer-events-none absolute -top-24 -right-24 size-96 rounded-full bg-primary-foreground/10 blur-3xl"
        />
        <div
          aria-hidden
          className="pointer-events-none absolute -bottom-32 -left-16 size-96 rounded-full bg-primary-foreground/10 blur-3xl"
        />

        <Logo inverted className="relative" />

        <div className="relative space-y-8">
          <div className="space-y-3">
            <h2 className="text-3xl font-semibold tracking-tight">Ask your documents anything.</h2>
            <p className="max-w-md text-primary-foreground/80">
              Upload a document, get a summary, and ask questions that are answered from the document
              itself.
            </p>
          </div>
          <ul className="space-y-6">
            {FEATURES.map(({ icon: Icon, title: featureTitle, text }) => (
              <li key={featureTitle} className="flex gap-4">
                <span className="grid size-10 shrink-0 place-items-center rounded-lg bg-primary-foreground/15">
                  <Icon className="size-5" />
                </span>
                <div>
                  <p className="font-medium">{featureTitle}</p>
                  <p className="text-sm text-primary-foreground/80">{text}</p>
                </div>
              </li>
            ))}
          </ul>
        </div>

        <p className="relative text-xs text-primary-foreground/70">
          Built with Go, PostgreSQL and React.
        </p>
      </aside>

      <div className="flex flex-col px-4 py-8 sm:px-6">
        <div className="flex items-center justify-between lg:justify-end">
          <Logo className="lg:hidden" />
          <ThemeToggle />
        </div>
        <main className="mx-auto flex w-full max-w-sm flex-1 flex-col justify-center py-12">
          <div className="mb-8 space-y-2">
            <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
            <p className="text-sm text-muted-foreground">{subtitle}</p>
          </div>
          {children}
          <p className="mt-8 text-center text-sm text-muted-foreground">{footer}</p>
        </main>
      </div>
    </div>
  )
}