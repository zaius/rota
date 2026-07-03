import { Loader2 } from "lucide-react"

/**
 * PageSpinner is the centered loading indicator every list page repeated while
 * its initial fetch was in flight.
 */
export function PageSpinner() {
  return (
    <div className="flex items-center justify-center h-64">
      <Loader2 className="h-8 w-8 animate-spin text-muted-foreground" />
    </div>
  )
}
