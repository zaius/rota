
import * as React from "react"
import { SystemMetrics } from "@/components/system-metrics"
import { api } from "@/lib/api"
import { SystemMetrics as SystemMetricsType } from "@/lib/types"
import { Loader2 } from "lucide-react"

export default function MetricsPage() {
  const [metrics, setMetrics] = React.useState<SystemMetricsType | null>(null)
  const [isLoading, setIsLoading] = React.useState(true)

  React.useEffect(() => {
    // clearInterval doesn't cancel a request already in flight, so a response
    // arriving after unmount would still call setState.
    let cancelled = false

    const fetchMetrics = async () => {
      try {
        const data = await api.getSystemMetrics()
        if (!cancelled) setMetrics(data)
      } catch (error) {
        if (!cancelled) console.error("Failed to fetch system metrics:", error)
      } finally {
        if (!cancelled) setIsLoading(false)
      }
    }

    fetchMetrics()

    // Refresh metrics every 5 seconds
    const interval = setInterval(fetchMetrics, 5000)

    return () => {
      cancelled = true
      clearInterval(interval)
    }
  }, [])

  if (isLoading || !metrics) {
    return (
      <div className="flex items-center justify-center h-96">
        <Loader2 className="h-8 w-8 animate-spin" />
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <SystemMetrics data={metrics} />
    </div>
  )
}
