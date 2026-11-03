import { useMemo } from "react"
import ReactMarkdown, { defaultUrlTransform, type Components } from "react-markdown"
import { cn } from "@/lib/utils"

const CITE = "cite:"

interface MarkdownProps {
  children: string
  className?: string
  /** Called with the passage number when an inline [1] chip is clicked. */
  onCitationClick?: (number: number) => void
}

export function Markdown({ children: source, className, onCitationClick }: MarkdownProps) {
  const components = useMemo<Components>(
    () => ({
      p: ({ children }) => <p className="leading-relaxed [&:not(:first-child)]:mt-3">{children}</p>,
      h1: ({ children }) => <h3 className="mt-4 mb-2 text-base font-semibold first:mt-0">{children}</h3>,
      h2: ({ children }) => <h3 className="mt-4 mb-2 text-base font-semibold first:mt-0">{children}</h3>,
      h3: ({ children }) => <h4 className="mt-4 mb-2 text-sm font-semibold first:mt-0">{children}</h4>,
      ul: ({ children }) => <ul className="my-3 list-disc space-y-1 pl-5">{children}</ul>,
      ol: ({ children }) => <ol className="my-3 list-decimal space-y-1 pl-5">{children}</ol>,
      li: ({ children }) => <li className="leading-relaxed">{children}</li>,
      strong: ({ children }) => <strong className="font-semibold">{children}</strong>,
      blockquote: ({ children }) => (
        <blockquote className="my-3 border-l-2 border-border pl-4 text-muted-foreground">{children}</blockquote>
      ),
      hr: () => <hr className="my-4 border-border" />,
      pre: ({ children }) => (
        <pre className="my-3 overflow-x-auto rounded-lg bg-background p-3 text-xs">{children}</pre>
      ),
      code: ({ className: codeClass, children }) => {
        const isBlock = /language-/.test(codeClass ?? "") || String(children).includes("\n")
        return isBlock ? (
          <code className="font-mono">{children}</code>
        ) : (
          <code className="rounded bg-background px-1 py-0.5 font-mono text-[0.85em]">{children}</code>
        )
      },
      a: ({ href, children }) => {
        if (href?.startsWith(CITE)) {
          const n = Number(href.slice(CITE.length))
          return (
            <button
              type="button"
              onClick={() => onCitationClick?.(n)}
              aria-label={`Show source ${n}`}
              className="mx-0.5 inline-flex h-5 min-w-5 items-center justify-center rounded-md bg-primary/10 px-1 text-xs font-medium text-primary transition-colors outline-none hover:bg-primary/20 focus-visible:ring-[3px] focus-visible:ring-ring/50"
            >
              {children}
            </button>
          )
        }
        return (
          <a
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            className="font-medium text-primary underline underline-offset-4"
          >
            {children}
          </a>
        )
      },
    }),
    [onCitationClick],
  )

  return (
    <div className={cn("break-words", className)}>
      <ReactMarkdown
        components={components}
        // The default transform drops unknown schemes; let our own "cite:" through.
        urlTransform={(url) => (url.startsWith(CITE) ? url : defaultUrlTransform(url))}
      >
        {source}
      </ReactMarkdown>
    </div>
  )
}