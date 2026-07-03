import type { LucideIcon } from "lucide-react"
import { Card, CardContent } from "@/components/ui/card"

/**
 * EmptyState is the icon + message + action card shown when a list is empty.
 */
export function EmptyState({
  icon: Icon,
  message,
  action,
}: {
  icon: LucideIcon
  message: string
  action?: React.ReactNode
}) {
  return (
    <Card>
      <CardContent className="flex flex-col items-center justify-center py-16 gap-3">
        <Icon className="h-10 w-10 text-muted-foreground" />
        <p className="text-muted-foreground">{message}</p>
        {action}
      </CardContent>
    </Card>
  )
}
