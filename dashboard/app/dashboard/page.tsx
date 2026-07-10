
import * as React from "react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Activity,
  CheckCircle2,
  Clock,
  TrendingUp,
  TrendingDown,
  Network,
} from "lucide-react"
import { Status, StatusIndicator, StatusLabel } from "@/components/ui/shadcn-io/status"
import { Area, AreaChart, CartesianGrid, ComposedChart, Line, XAxis, YAxis } from "recharts"
import {
  ChartConfig,
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart"
import { Button } from "@/components/ui/button"
import { api } from "@/lib/api"
import { ChartRange, DashboardStats, TrafficPoint } from "@/lib/types"

// Colors are validated (dataviz six checks) per mode: light mode uses the
// theme tokens as-is; dark mode uses darker steps of the same hues because the
// theme's dark tokens fall outside the chart lightness band on this surface.
const chartConfig = {
  successes: {
    label: "Successful",
    theme: { light: "var(--chart-2)", dark: "oklch(0.62 0.17 162.48)" },
  },
  failures: {
    label: "Failed",
    theme: { light: "var(--destructive)", dark: "oklch(0.62 0.191 22.216)" },
  },
  successRate: {
    label: "Success rate",
    theme: { light: "var(--chart-2)", dark: "oklch(0.62 0.17 162.48)" },
  },
  p50: {
    label: "p50 latency",
    theme: { light: "var(--chart-1)", dark: "oklch(0.58 0.21 264.376)" },
  },
  p95: {
    label: "p95 latency",
    theme: { light: "var(--chart-1)", dark: "oklch(0.58 0.21 264.376)" },
  },
} satisfies ChartConfig

const RANGES: ChartRange[] = ["1h", "6h", "24h", "7d", "30d"]

// One row per bucket, with client-derived series: failure counts for the
// stacked traffic chart, success percentage, and null latency where a bucket
// had no successful requests (nulls render as gaps, not fake zeros).
interface TrafficRow {
  time: string
  requests: number
  successes: number
  failures: number
  successRate: number | null
  p50: number | null
  p95: number | null
}

function toRows(points: TrafficPoint[]): TrafficRow[] {
  return points.map((p) => ({
    time: p.time,
    requests: p.requests,
    successes: p.successes,
    failures: p.requests - p.successes,
    successRate: p.requests > 0 ? Math.round((p.successes / p.requests) * 1000) / 10 : null,
    p50: p.successes > 0 ? p.p50_ms : null,
    p95: p.successes > 0 ? p.p95_ms : null,
  }))
}

// Axis tick labels: time-of-day within a day, day-of-month beyond it.
function tickFormatter(range: ChartRange): (iso: string) => string {
  return (iso) => {
    const d = new Date(iso)
    if (range === "30d") return d.toLocaleDateString(undefined, { month: "short", day: "numeric" })
    if (range === "7d")
      return d.toLocaleDateString(undefined, { weekday: "short" }) +
        " " + d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })
  }
}

// Tooltip header: always the full local timestamp.
const tooltipLabel = (iso: string) =>
  new Date(iso).toLocaleString(undefined, {
    month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
  })

export default function DashboardPage() {
  const [stats, setStats] = React.useState<DashboardStats | null>(null)
  const [range, setRange] = React.useState<ChartRange>("24h")
  const [traffic, setTraffic] = React.useState<TrafficRow[]>([])
  const [isLoading, setIsLoading] = React.useState(true)

  React.useEffect(() => {
    let ws: WebSocket | null = null

    const fetchStats = async () => {
      try {
        setStats(await api.getDashboardStats())
      } catch (error) {
        console.error("Failed to fetch dashboard stats:", error)
      } finally {
        setIsLoading(false)
      }
    }

    fetchStats()

    // Setup WebSocket for real-time updates
    try {
      ws = api.createDashboardWebSocket((data) => {
        setStats(data)
      })
    } catch (error) {
      console.error("Failed to connect to WebSocket:", error)
    }

    return () => {
      if (ws) {
        ws.close()
      }
    }
  }, [])

  // The shared range drives every chart; one request feeds all three.
  React.useEffect(() => {
    let cancelled = false
    api
      .getTrafficChart(range)
      .then((res) => {
        if (!cancelled) setTraffic(toRows(res.data))
      })
      .catch((error) => console.error("Failed to fetch traffic chart:", error))
    return () => {
      cancelled = true
    }
  }, [range])

  const fmtTick = tickFormatter(range)

  if (isLoading || !stats) {
    return (
      <div className="space-y-4">
        <div className="flex items-center justify-between">
          <div>
            <h1 className="text-3xl font-bold tracking-tight">Dashboard</h1>
            <p className="text-muted-foreground">Loading...</p>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">Dashboard</h1>
          <p className="text-muted-foreground">
            Real-time monitoring and statistics
          </p>
        </div>
        <Status status="online">
          <StatusIndicator />
          <StatusLabel>Live</StatusLabel>
        </Status>
      </div>

      {/* Stats Cards */}
      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Active Proxies</CardTitle>
            <Network className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{stats.active_proxies}/{stats.total_proxies}</div>
            <p className="text-xs text-muted-foreground">
              {Math.round((stats.active_proxies / stats.total_proxies) * 100)}% operational
            </p>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Total Requests</CardTitle>
            <Activity className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold" suppressHydrationWarning>
              {stats.total_requests.toLocaleString('en-US')}
            </div>
            <p className="text-xs text-muted-foreground">
              {stats.request_growth >= 0 ? (
                <TrendingUp className="inline h-3 w-3" />
              ) : (
                <TrendingDown className="inline h-3 w-3" />
              )}{" "}
              {stats.request_growth >= 0 ? "+" : ""}{stats.request_growth.toFixed(1)}% from yesterday
            </p>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Success Rate</CardTitle>
            <CheckCircle2 className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{stats.avg_success_rate.toFixed(1)}%</div>
            <p className="text-xs text-muted-foreground">
              {stats.success_rate_growth >= 0 ? (
                <TrendingUp className="inline h-3 w-3" />
              ) : (
                <TrendingDown className="inline h-3 w-3" />
              )}{" "}
              {stats.success_rate_growth >= 0 ? "+" : ""}{stats.success_rate_growth.toFixed(1)}% from yesterday
            </p>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Avg Response Time</CardTitle>
            <Clock className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{stats.avg_response_time}ms</div>
            <p className="text-xs text-muted-foreground">
              {stats.response_time_delta <= 0 ? (
                <TrendingDown className="inline h-3 w-3" />
              ) : (
                <TrendingUp className="inline h-3 w-3" />
              )}{" "}
              {stats.response_time_delta <= 0 ? "" : "+"}{stats.response_time_delta}ms from yesterday
            </p>
          </CardContent>
        </Card>
      </div>

      {/* Charts */}
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold tracking-tight">Traffic</h2>
        <div className="flex gap-1 rounded-lg border p-1">
          {RANGES.map((r) => (
            <Button
              key={r}
              size="sm"
              variant={range === r ? "secondary" : "ghost"}
              className="h-7 px-2.5"
              onClick={() => setRange(r)}
            >
              {r}
            </Button>
          ))}
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Requests</CardTitle>
          <CardDescription>Successful and failed requests per interval</CardDescription>
        </CardHeader>
        <CardContent>
          <ChartContainer config={chartConfig} className="h-[260px] w-full">
            <AreaChart data={traffic}>
              <CartesianGrid strokeDasharray="3 3" vertical={false} />
              <XAxis
                dataKey="time"
                tickLine={false}
                axisLine={false}
                tickMargin={8}
                minTickGap={32}
                tickFormatter={fmtTick}
              />
              <YAxis tickLine={false} axisLine={false} tickMargin={8} allowDecimals={false} />
              <ChartTooltip content={<ChartTooltipContent labelFormatter={tooltipLabel} />} />
              <ChartLegend content={<ChartLegendContent />} />
              <Area
                type="linear"
                dataKey="successes"
                stackId="1"
                stroke="var(--color-successes)"
                strokeWidth={2}
                fill="var(--color-successes)"
                fillOpacity={0.3}
                isAnimationActive={false}
              />
              <Area
                type="linear"
                dataKey="failures"
                stackId="1"
                stroke="var(--color-failures)"
                strokeWidth={2}
                fill="var(--color-failures)"
                fillOpacity={0.3}
                isAnimationActive={false}
              />
            </AreaChart>
          </ChartContainer>
        </CardContent>
      </Card>

      <div className="grid gap-4 md:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Success Rate</CardTitle>
            <CardDescription>Share of requests that succeeded per interval</CardDescription>
          </CardHeader>
          <CardContent>
            <ChartContainer config={chartConfig} className="h-[220px] w-full">
              <AreaChart data={traffic}>
                <CartesianGrid strokeDasharray="3 3" vertical={false} />
                <XAxis
                  dataKey="time"
                  tickLine={false}
                  axisLine={false}
                  tickMargin={8}
                  minTickGap={32}
                  tickFormatter={fmtTick}
                />
                <YAxis
                  domain={[0, 100]}
                  tickLine={false}
                  axisLine={false}
                  tickMargin={8}
                  tickFormatter={(value) => `${value}%`}
                />
                <ChartTooltip content={<ChartTooltipContent labelFormatter={tooltipLabel} />} />
                <Area
                  type="linear"
                  dataKey="successRate"
                  stroke="var(--color-successRate)"
                  strokeWidth={2}
                  fill="var(--color-successRate)"
                  fillOpacity={0.15}
                  connectNulls={false}
                  isAnimationActive={false}
                />
              </AreaChart>
            </ChartContainer>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Response Time</CardTitle>
            <CardDescription>Latency percentiles of successful requests</CardDescription>
          </CardHeader>
          <CardContent>
            <ChartContainer config={chartConfig} className="h-[220px] w-full">
              <ComposedChart data={traffic}>
                <CartesianGrid strokeDasharray="3 3" vertical={false} />
                <XAxis
                  dataKey="time"
                  tickLine={false}
                  axisLine={false}
                  tickMargin={8}
                  minTickGap={32}
                  tickFormatter={fmtTick}
                />
                <YAxis
                  tickLine={false}
                  axisLine={false}
                  tickMargin={8}
                  tickFormatter={(value) => `${value}ms`}
                />
                <ChartTooltip content={<ChartTooltipContent labelFormatter={tooltipLabel} />} />
                <ChartLegend content={<ChartLegendContent />} />
                <Area
                  type="linear"
                  dataKey="p95"
                  stroke="var(--color-p95)"
                  strokeWidth={2}
                  strokeDasharray="4 4"
                  fill="var(--color-p95)"
                  fillOpacity={0.12}
                  connectNulls={false}
                  isAnimationActive={false}
                />
                <Line
                  type="linear"
                  dataKey="p50"
                  stroke="var(--color-p50)"
                  strokeWidth={2}
                  dot={false}
                  connectNulls={false}
                  isAnimationActive={false}
                />
              </ComposedChart>
            </ChartContainer>
          </CardContent>
        </Card>
      </div>

    </div>
  )
}
