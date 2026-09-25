import * as React from "react"
import { Link, type To } from "react-router-dom"
import { ChevronDown, Search } from "lucide-react"
import { cn } from "@/lib/utils"

const control =
  "border-border bg-background h-8 rounded-md border font-medium transition-colors focus-visible:ring-ring focus-visible:ring-2 focus-visible:outline-none disabled:opacity-50"

/** Search box with the icon inside; commits on Enter or after a pause. */
export function SearchInput({
  value,
  onChange,
  placeholder = "Search…",
  className,
  delay = 400,
  ...rest
}: {
  value: string
  onChange: (v: string) => void
  placeholder?: string
  className?: string
  delay?: number
} & Omit<React.ComponentProps<"input">, "value" | "onChange">) {
  const [draft, setDraft] = React.useState(value)
  // Only adopt an external `value` (back button, "Clear") — never one we sent
  // ourselves, otherwise a slow URL round-trip would overwrite what the user
  // typed in the meantime.
  const sent = React.useRef(value)
  React.useEffect(() => {
    if (value !== sent.current) {
      sent.current = value
      setDraft(value)
    }
  }, [value])
  React.useEffect(() => {
    if (draft === sent.current) return
    const t = setTimeout(() => {
      sent.current = draft
      onChange(draft)
    }, delay)
    return () => clearTimeout(t)
  }, [draft, onChange, delay])
  return (
    <div className={cn("relative", className)}>
      <Search className="text-muted-foreground pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2" aria-hidden />
      <input
        type="search"
        value={draft}
        placeholder={placeholder}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            sent.current = draft
            onChange(draft)
          }
        }}
        className={cn(control, "placeholder:text-muted-foreground w-56 pr-2.5 pl-8 font-normal")}
        {...rest}
      />
    </div>
  )
}

/** Native select styled as a control; changing it applies immediately. */
export function NativeSelect({
  value,
  onChange,
  options,
  className,
  "aria-label": ariaLabel,
  disabled,
}: {
  value: string
  onChange: (v: string) => void
  options: { value: string; label: string; disabled?: boolean }[]
  className?: string
  "aria-label"?: string
  disabled?: boolean
}) {
  return (
    <div className={cn("relative inline-flex", className)}>
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        aria-label={ariaLabel}
        disabled={disabled}
        className={cn(control, "appearance-none pr-7 pl-2.5")}
      >
        {options.map((o) => (
          <option key={o.value} value={o.value} disabled={o.disabled}>
            {o.label}
          </option>
        ))}
      </select>
      <ChevronDown className="text-muted-foreground pointer-events-none absolute top-1/2 right-2 size-3.5 -translate-y-1/2" aria-hidden />
    </div>
  )
}

/** Segmented control made of links (or buttons when no href). */
export function Segment({
  items,
  label,
}: {
  items: { label: React.ReactNode; active: boolean; to?: To; onClick?: () => void }[]
  label: string
}) {
  return (
    <div className="border-border inline-flex overflow-hidden rounded-md border" role="group" aria-label={label}>
      {items.map((it, i) => {
        const cls = cn(
          "px-2.5 py-1 font-medium transition-colors focus-visible:ring-ring focus-visible:ring-2 focus-visible:outline-none focus-visible:ring-inset",
          i > 0 && "border-border border-l",
          it.active ? "bg-accent text-foreground" : "text-muted-foreground hover:text-foreground hover:bg-accent/50"
        )
        return it.to ? (
          <Link key={i} to={it.to} aria-current={it.active ? "true" : undefined} className={cls}>
            {it.label}
          </Link>
        ) : (
          <button key={i} type="button" onClick={it.onClick} aria-pressed={it.active} className={cls}>
            {it.label}
          </button>
        )
      })}
    </div>
  )
}

/** A control-height link/button that reads as a secondary action. */
export const controlClass = control
