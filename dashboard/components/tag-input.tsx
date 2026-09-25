import * as React from "react"
import { X } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"

interface TagInputProps {
  value: string[]
  onChange: (tags: string[]) => void
  /** Known values offered as one-click suggestions; ones already added are hidden. */
  suggestions?: string[]
  placeholder?: string
  emptyText?: string
  disabled?: boolean
  id?: string
}

// Chip-style list editor: Enter or comma adds the draft, × or Backspace on an
// empty draft removes a chip.
export function TagInput({
  value,
  onChange,
  suggestions = [],
  placeholder = "Add tag…",
  emptyText = "No tags",
  disabled,
  id,
}: TagInputProps) {
  const [draft, setDraft] = React.useState("")

  const addTag = (raw: string) => {
    const tag = raw.trim()
    if (!tag || value.includes(tag)) return
    onChange([...value, tag])
  }

  const removeTag = (tag: string) => onChange(value.filter(t => t !== tag))

  const commitDraft = () => {
    if (draft.trim()) {
      addTag(draft)
      setDraft("")
    }
  }

  const available = suggestions.filter(s => !value.includes(s))

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex min-h-[28px] flex-wrap items-center gap-1.5">
        {value.length === 0 && (
          <span className="text-xs italic text-muted-foreground">{emptyText}</span>
        )}
        {value.map(tag => (
          <Badge key={tag} variant="secondary" className="gap-1 pr-1">
            <span className="text-xs">{tag}</span>
            <button
              type="button"
              className="rounded p-0.5 hover:bg-muted-foreground/20"
              onClick={() => removeTag(tag)}
              disabled={disabled}
              aria-label={`Remove ${tag}`}
            >
              <X className="h-3 w-3" />
            </button>
          </Badge>
        ))}
      </div>
      <Input
        id={id}
        value={draft}
        placeholder={placeholder}
        disabled={disabled}
        onChange={e => setDraft(e.target.value)}
        onKeyDown={e => {
          if (e.key === "Enter" || e.key === ",") {
            e.preventDefault()
            commitDraft()
          } else if (e.key === "Backspace" && !draft && value.length > 0) {
            removeTag(value[value.length - 1])
          }
        }}
        onBlur={commitDraft}
      />
      {available.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {available.slice(0, 12).map(s => (
            <Button
              key={s}
              type="button"
              size="sm"
              variant="outline"
              disabled={disabled}
              className="h-6 px-2 text-xs"
              onClick={() => addTag(s)}
            >
              {s}
            </Button>
          ))}
        </div>
      )}
    </div>
  )
}
