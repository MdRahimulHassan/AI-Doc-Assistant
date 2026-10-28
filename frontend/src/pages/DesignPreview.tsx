import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { StatusBadge } from "@/components/StatusBadge"
import { ThemeToggle } from "@/components/ThemeToggle"

const SWATCHES = [
  ["bg-background", "background"],
  ["bg-card", "card"],
  ["bg-primary", "primary"],
  ["bg-secondary", "secondary"],
  ["bg-muted", "muted"],
  ["bg-accent", "accent"],
  ["bg-destructive", "destructive"],
  ["bg-success", "success"],
  ["bg-warning", "warning"],
  ["bg-info", "info"],
]

export default function DesignPreview() {
  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="border-b border-border">
        <div className="mx-auto flex h-14 max-w-6xl items-center justify-between px-4 sm:px-6">
          <span className="font-semibold tracking-tight">Design preview</span>
          <ThemeToggle />
        </div>
      </header>

      <main className="mx-auto max-w-6xl space-y-8 px-4 py-8 sm:px-6">
        <section className="space-y-2">
          <h1 className="text-2xl font-semibold tracking-tight">Typography</h1>
          <h2 className="text-lg font-semibold">Section title</h2>
          <p className="text-sm">Body text sits at 14px and carries most of the interface.</p>
          <p className="text-sm text-muted-foreground">Secondary text for hints and metadata.</p>
          <p className="text-xs text-muted-foreground">Caption text, 12px.</p>
        </section>

        <section className="space-y-3">
          <h2 className="text-lg font-semibold">Colour tokens</h2>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
            {SWATCHES.map(([cls, name]) => (
              <div key={name} className="space-y-1">
                <div className={`h-12 rounded-md border border-border ${cls}`} />
                <p className="text-xs text-muted-foreground">{name}</p>
              </div>
            ))}
          </div>
        </section>

        <section className="space-y-3">
          <h2 className="text-lg font-semibold">Buttons</h2>
          <div className="flex flex-wrap gap-3">
            <Button>Primary</Button>
            <Button variant="secondary">Secondary</Button>
            <Button variant="outline">Outline</Button>
            <Button variant="ghost">Ghost</Button>
            <Button variant="destructive">Delete</Button>
            <Button disabled>Disabled</Button>
            <Button onClick={() => toast.success("Toast works in both themes")}>
              Show toast
            </Button>
          </div>
        </section>

        <section className="space-y-3">
          <h2 className="text-lg font-semibold">Badges</h2>
          <div className="flex flex-wrap gap-2">
            <Badge>Default</Badge>
            <Badge variant="secondary">Secondary</Badge>
            <Badge variant="outline">Outline</Badge>
            <StatusBadge status="processing" />
            <StatusBadge status="ready" />
            <StatusBadge status="failed" />
            <StatusBadge status="none" />
          </div>
        </section>

        <section className="grid gap-6 md:grid-cols-2">
          <Card>
            <CardHeader>
              <CardTitle>Sign in</CardTitle>
              <CardDescription>Inputs, labels and a validation error.</CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="space-y-2">
                <Label htmlFor="email">Email</Label>
                <Input id="email" type="email" placeholder="you@example.com" />
              </div>
              <div className="space-y-2">
                <Label htmlFor="password">Password</Label>
                <Input id="password" type="password" aria-invalid="true" />
                <p className="text-xs text-destructive">Password is required.</p>
              </div>
              <Button className="w-full">Sign in</Button>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Loading state</CardTitle>
              <CardDescription>Skeletons beat spinners for content.</CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              <Skeleton className="h-4 w-2/3" />
              <Skeleton className="h-4 w-full" />
              <Skeleton className="h-4 w-5/6" />
            </CardContent>
          </Card>
        </section>
      </main>
    </div>
  )
}