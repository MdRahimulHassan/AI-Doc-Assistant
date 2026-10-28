import { useEffect, useState, type ReactNode } from "react"
import { ThemeContext, type Theme } from "./theme-context"

const STORAGE_KEY = "theme"

function readStoredTheme(): Theme {
  try {
    const value = localStorage.getItem(STORAGE_KEY)
    if (value === "light" || value === "dark" || value === "system") return value
  } catch {
    /* storage unavailable: fall through to the default */
  }
  return "system"
}

function applyTheme(theme: Theme) {
  const dark =
    theme === "dark" ||
    (theme === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches)
  document.documentElement.classList.toggle("dark", dark)
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setThemeState] = useState<Theme>(readStoredTheme)

  useEffect(() => {
    applyTheme(theme)
    if (theme !== "system") return

    // In "system" mode, follow the OS setting live.
    const query = window.matchMedia("(prefers-color-scheme: dark)")
    const onChange = () => applyTheme("system")
    query.addEventListener("change", onChange)
    return () => query.removeEventListener("change", onChange)
  }, [theme])

  const setTheme = (next: Theme) => {
    try {
      localStorage.setItem(STORAGE_KEY, next)
    } catch {
      /* ignore: the theme still changes for this session */
    }
    setThemeState(next)
  }

  return (
    <ThemeContext.Provider value={{ theme, setTheme }}>
      {children}
    </ThemeContext.Provider>
  )
}