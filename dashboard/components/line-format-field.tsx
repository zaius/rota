import * as React from "react"
import { ChevronDown, Check, Trash2, AlertCircle } from "lucide-react"
import { api } from "@/lib/api"
import { useResourceQuery } from "@/hooks/use-resource-query"
import {
  compileLineFormat, FORMAT_PRESETS, FORMAT_URL, isPresetFormat,
} from "@/lib/lineformat"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  DropdownMenu, DropdownMenuTrigger, DropdownMenuContent,
  DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator,
} from "@/components/ui/dropdown-menu"

interface LineFormatFieldProps {
  value: string
  onChange: (format: string) => void
  // Optional sample line to preview the current format against.
  sampleLine?: string
  disabled?: boolean
  id?: string
}

// LineFormatField is the shared editor for a proxy-list line format. It offers
// a free-form text field prefilled with the format, a dropdown of built-in
// presets plus previously-used custom formats, live validation, and a parsed
// preview so an error-prone template is caught before saving.
export function LineFormatField({
  value, onChange, sampleLine, disabled, id,
}: LineFormatFieldProps) {
  const historyQuery = useResourceQuery(
    ["format-history"],
    () => api.getFormatHistory().then(r => r.formats),
  )
  const history = historyQuery.data ?? []

  const compiled = React.useMemo(() => compileLineFormat(value), [value])

  // When the caller has actual list text, preview the current format against
  // its first non-comment line so the user sees exactly what gets extracted.
  const preview = React.useMemo(() => {
    if (compiled.error || !sampleLine) return null
    const line = sampleLine
      .split("\n")
      .map(l => l.trim())
      .find(l => l !== "" && !l.startsWith("#"))
    if (!line) return null
    return { line, parsed: compiled.parse(line) }
  }, [compiled, sampleLine])

  const pick = (format: string) => onChange(format)

  const removeHistory = async (e: React.MouseEvent, entryId: number) => {
    e.preventDefault()
    e.stopPropagation()
    try {
      await api.deleteFormatHistory(entryId)
      historyQuery.invalidate()
    } catch {
      /* non-fatal: leave the entry in place */
    }
  }

  const invalid = value.trim() !== "" && compiled.error !== null

  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>Line format</Label>
      <div className="flex gap-2">
        <Input
          id={id}
          value={value}
          spellCheck={false}
          autoComplete="off"
          className="font-mono text-xs"
          placeholder={FORMAT_URL}
          onChange={e => onChange(e.target.value)}
          disabled={disabled}
          aria-invalid={invalid}
        />
        <DropdownMenu>
          <DropdownMenuTrigger
            disabled={disabled}
            className="inline-flex items-center gap-1 rounded-md border bg-transparent px-3 text-sm shadow-xs whitespace-nowrap hover:bg-accent disabled:opacity-50"
          >
            Presets <ChevronDown className="h-4 w-4" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="max-w-[min(90vw,26rem)]">
            <DropdownMenuLabel>Presets</DropdownMenuLabel>
            {FORMAT_PRESETS.map(p => (
              <DropdownMenuItem key={p.value} onSelect={() => pick(p.value)} className="flex-col items-start gap-0.5">
                <span className="flex w-full items-center gap-2">
                  {value.trim() === p.value && <Check className="h-3 w-3 shrink-0" />}
                  <code className="text-xs">{p.label}</code>
                </span>
                <span className="text-[11px] text-muted-foreground">{p.hint}</span>
              </DropdownMenuItem>
            ))}
            {history.length > 0 && (
              <>
                <DropdownMenuSeparator />
                <DropdownMenuLabel>Recently used</DropdownMenuLabel>
                {history.map(h => (
                  <DropdownMenuItem
                    key={h.id}
                    onSelect={() => pick(h.format)}
                    className="flex items-center justify-between gap-2"
                  >
                    <span className="flex items-center gap-2 min-w-0">
                      {value.trim() === h.format && <Check className="h-3 w-3 shrink-0" />}
                      <code className="text-xs truncate">{h.format}</code>
                    </span>
                    <button
                      type="button"
                      onClick={e => removeHistory(e, h.id)}
                      className="text-muted-foreground hover:text-red-500 shrink-0"
                      title="Remove from history"
                    >
                      <Trash2 className="h-3 w-3" />
                    </button>
                  </DropdownMenuItem>
                ))}
              </>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      {/* Validation + preview */}
      {invalid ? (
        <p className="flex items-center gap-1.5 text-xs text-red-500">
          <AlertCircle className="h-3 w-3 shrink-0" />
          {compiled.error}
        </p>
      ) : preview && preview.parsed ? (
        <p className="text-xs text-muted-foreground">
          <code className="text-[11px]">{preview.line}</code>
          {" → "}
          <span className="text-foreground">
            {preview.parsed.protocol ? `${preview.parsed.protocol}://` : ""}
            {preview.parsed.username ? `${preview.parsed.username}:••@` : ""}
            {preview.parsed.address}
          </span>
        </p>
      ) : preview && !preview.parsed ? (
        <p className="flex items-center gap-1.5 text-xs text-amber-500">
          <AlertCircle className="h-3 w-3 shrink-0" />
          This format doesn't match <code className="text-[11px]">{preview.line}</code>
        </p>
      ) : (
        <p className="text-xs text-muted-foreground">
          {isPresetFormat(value)
            ? "Fields host, port, user, pass, protocol, separated by any characters. Wrap optional parts in […]; use * to skip a column."
            : "Custom template — e.g. host:port:user:pass or host:port:*:user:pass."}
        </p>
      )}
    </div>
  )
}
