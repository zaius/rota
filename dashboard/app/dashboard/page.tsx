import * as React from "react"
import { Link } from "react-router-dom"
import { Radio, RadioTower } from "lucide-react"
import { Area, AreaChart, Bar, BarChart, CartesianGrid, ComposedChart, Line, XAxis, YAxis } from "recharts"
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from "@/components/ui/chart"
import { PageHeader, Section, Footnote, LoadingLine } from "@/components/page-header"
import { StatStrip, type Stat } from "@/components/stat-strip"
import { Segment } from "@/components/controls"
import { StatusLabel } from "@/components/status"
import { SplitBar } from "@/components/usage-bar"
import { DomainStats } from "@/components/domain-stats"
import { useUrlState } from "@/hooks/use-url-state"
import { api } from "@/lib/api"
import { bytes, count, ms, percent, signedPercent } from "@/lib/format"
import type { ChartRange, DashboardStats, TrafficPoint } from "@/lib/types"

const RANGES: { value: ChartRange; hint: string }[] = [
  { value: "1h", hint: "Minute buckets over the last hour." },
  { value: "6h", hint: "5-minute buckets over the last 6 hours." },
  { value: "24h", hint: "30-minute buckets over the last 24 hours." },
  { value: "7d", hint: "4-hour buckets over the last 7 days." },
  { value: "30d", hint: "Daily buckets over the last 30 days." },
]

const requestsConfig = {
  successes: { label: "Succeeded", color: "var(--good)" },
  failures: { label: "Failed", color: "var(--critical)" },
} satisfies ChartConfig

const successConfig = {
  successRate: { label: "Success rate", color: "var(--chart-3)" },
} satisfies ChartConfig

const latencyConfig = {
  p50: { label: "p50", color: "var(--chart-1)" },
  p95: { label: "p95", color: "var(--chart-2)" },
} satisfies ChartConfig

const URL_DEFAULTS = { range: "24h" }

// One row per bucket, with client-derived series: failure counts for the
// stacked requests chart, success percentage, and null latency where a bucket
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

// Axis tick labels: time of day within a day, day of month beyond it.
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

const axisTick = { fill: "var(--muted-foreground)", fontSize: 11 }

// loneDot draws a dot only on a point with no neighbor to join: a lone value
// between gaps otherwise renders as nothing at all.
function loneDot(rows: TrafficRow[], key: "successRate" | "p50" | "p95", color: string) {
  return function LoneDot({ cx, cy, index = 0 }: { cx?: number; cy?: number; index?: number }) {
    const lone = rows[index]?.[key] != null && rows[index - 1]?.[key] == null && rows[index + 1]?.[key] == null
    if (!lone || cx == null || cy == null) return <g key={index} />
    return <circle key={index} cx={cx} cy={cy} r={2.5} fill={color} />
  }
}

export default function OverviewPage() {
  const [url] = useUrlState(URL_DEFAULTS)
  const range = RANGES.find((r) => r.value === url.range) ?? RANGES[2]

  const [stats, setStats] = React.useState<DashboardStats | null>(null)
  const [traffic, setTraffic] = React.useState<TrafficRow[]>([])
  const [live, setLive] = React.useState(false)
  const [loading, setLoading] = React.useState(true)

  React.useEffect(() => {
    let cancelled = false
    api
      .getDashboardStats()
      .then((s) => !cancelled && setStats(s))
      .catch((error) => console.error("Failed to fetch dashboard stats:", error))
      .finally(() => !cancelled && setLoading(false))

    const ws = api.createDashboardWebSocket(setStats, setLive)
    return () => {
      cancelled = true
      ws.close()
    }
  }, [])

  // The shared range drives every chart; one request feeds all three.
  React.useEffect(() => {
    let cancelled = false
    api
      .getTrafficChart(range.value)
      .then((res) => !cancelled && setTraffic(toRows(res.data)))
      .catch((error) => console.error("Failed to fetch traffic chart:", error))
    return () => {
      cancelled = true
    }
  }, [range.value])

  if (loading && !stats) return <LoadingLine />

  const rate = stats?.avg_success_rate ?? 0
  // Thresholds only mean something once there is traffic to measure.
  const rateTone: Stat["tone"] =
    !stats || stats.total_requests === 0 ? "default" : rate >= 90 ? "good" : rate >= 70 ? "warning" : "critical"
  const operational = stats && stats.total_proxies > 0 ? (stats.active_proxies / stats.total_proxies) * 100 : 0
  const tunnels = stats?.tunnels

  const headline: Stat[] = [
    {
      label: "Active proxies",
      value: stats ? count(stats.active_proxies) : "—",
      hint: stats ? `of ${count(stats.total_proxies)} in inventory · ${Math.round(operational)}% usable` : undefined,
    },
    {
      label: "Requests, all time",
      value: stats ? count(stats.total_requests) : "—",
      // request_growth compares the last 24h with the 24h before it.
      hint: stats ? `last 24h ${signedPercent(stats.request_growth)} vs the 24h before` : undefined,
    },
    {
      label: "Success rate",
      value: stats ? percent(stats.avg_success_rate) : "—",
      hint: stats ? `${signedPercent(stats.success_rate_growth)} vs yesterday` : undefined,
      tone: rateTone,
    },
    {
      label: "Avg response",
      value: stats ? ms(stats.avg_response_time) : "—",
      hint: stats
        ? `${stats.response_time_delta > 0 ? "+" : stats.response_time_delta < 0 ? "−" : ""}${count(Math.abs(stats.response_time_delta))} ms vs yesterday`
        : undefined,
    },
    // A CONNECT tunnel is one event however many requests the client sends
    // through it, so without these HTTPS traffic reads as near-zero volume.
    {
      label: "HTTPS tunnels, 24h",
      value: tunnels ? count(tunnels.today) : "—",
      hint: tunnels ? `${count(tunnels.open)} open now · ${tunnels.mean_concurrency.toFixed(1)} on average` : undefined,
    },
    {
      label: "Tunnel data, 24h",
      value: tunnels ? bytes(tunnels.bytes_up_today + tunnels.bytes_down_today) : "—",
      hint: tunnels
        ? `${bytes(tunnels.bytes_up_today)} sent · ${bytes(tunnels.bytes_down_today)} received · ${
            tunnels.requests_today > 0 ? `${count(tunnels.requests_today)} requests inspected` : "contents not inspected"
          }`
        : undefined,
    },
  ]

  const fmtTick = tickFormatter(range.value)
  const hasTraffic = traffic.some((r) => r.requests > 0)
  const totalRequests = traffic.reduce((s, r) => s + r.requests, 0)
  const totalFailures = traffic.reduce((s, r) => s + r.failures, 0)
  const busiest = traffic.reduce<TrafficRow | null>((top, r) => (top === null || r.requests > top.requests ? r : top), null)
  const worst = traffic.reduce<TrafficRow | null>(
    (low, r) => (r.successRate !== null && (low === null || r.successRate < (low.successRate ?? 100)) ? r : low),
    null,
  )
  const hasLatency = traffic.some((r) => r.p50 !== null)
  const peakP95 = Math.max(0, ...traffic.map((r) => r.p95 ?? 0))
  const peakP50 = Math.max(0, ...traffic.map((r) => r.p50 ?? 0))

  const xAxis = (
    <XAxis
      dataKey="time"
      tickLine={false}
      axisLine={false}
      tickMargin={8}
      minTickGap={32}
      tickFormatter={fmtTick}
      tick={axisTick}
    />
  )
  const grid = <CartesianGrid vertical={false} stroke="var(--border)" strokeDasharray="2 4" />

  return (
    <>
      <PageHeader
        title="Overview"
        description="What the proxy fleet is doing right now. Headline numbers update live; charts and domains follow the selected range."
      >
        {live ? (
          <StatusLabel icon={RadioTower} tone="good">Live</StatusLabel>
        ) : (
          <StatusLabel icon={Radio} tone="muted">Reconnecting</StatusLabel>
        )}
        <Segment
          label="Chart range"
          items={RANGES.map((r) => ({
            label: r.value,
            active: range.value === r.value,
            to: r.value === URL_DEFAULTS.range ? "/dashboard" : `/dashboard?range=${r.value}`,
          }))}
        />
      </PageHeader>

      <StatStrip stats={headline} columns={6} />

      {stats && (
        <Section
          title="Fleet"
          description="Share of the inventory in each state. Failed proxies are retried on the next health check; idle ones have not been checked yet."
        >
          <SplitBar
            segments={[
              { value: stats.active_proxies, tone: "good", label: "Active" },
              { value: Math.max(0, stats.total_proxies - stats.active_proxies), tone: "muted", label: "Failed or idle" },
            ]}
          />
        </Section>
      )}

      <Section title="Requests" description={`${range.hint} Succeeded and failed requests per bucket.`}>
        {!hasTraffic ? (
          <p className="text-muted-foreground py-8 text-center">No requests in this range.</p>
        ) : (
          <ChartContainer config={requestsConfig} className="aspect-auto h-[11rem] w-full">
            <BarChart data={traffic} margin={{ left: 0, right: 8, top: 8, bottom: 0 }} barCategoryGap="20%">
              {grid}
              {xAxis}
              <YAxis width={40} tickLine={false} axisLine={false} allowDecimals={false} tick={axisTick} />
              <ChartTooltip cursor={{ fill: "var(--accent)" }} content={<ChartTooltipContent labelFormatter={tooltipLabel} />} />
              <Bar dataKey="successes" stackId="a" fill="var(--color-successes)" isAnimationActive={false} />
              <Bar dataKey="failures" stackId="a" fill="var(--color-failures)" radius={[2, 2, 0, 0]} isAnimationActive={false} />
            </BarChart>
          </ChartContainer>
        )}
        <Footnote
          items={[
            { label: "Requests in range", value: count(totalRequests) },
            { label: "Failed", value: totalRequests ? `${count(totalFailures)} · ${percent((totalFailures / totalRequests) * 100)}` : "—" },
            { label: "Busiest bucket", value: busiest && busiest.requests > 0 ? `${tooltipLabel(busiest.time)} · ${count(busiest.requests)}` : "—" },
          ]}
        />
      </Section>

      <Section title="Success rate" description={`${range.hint} Share of each bucket's requests that succeeded; empty buckets leave a gap.`}>
        {!hasTraffic ? (
          <p className="text-muted-foreground py-8 text-center">No requests in this range.</p>
        ) : (
          <ChartContainer config={successConfig} className="aspect-auto h-[9rem] w-full">
            <AreaChart data={traffic} margin={{ left: 0, right: 8, top: 8, bottom: 0 }}>
              <defs>
                <linearGradient id="fill-success" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stopColor="var(--color-successRate)" stopOpacity={0.22} />
                  <stop offset="100%" stopColor="var(--color-successRate)" stopOpacity={0.02} />
                </linearGradient>
              </defs>
              {grid}
              {xAxis}
              <YAxis width={40} domain={[0, 100]} tickLine={false} axisLine={false} tickFormatter={(v) => `${v}%`} tick={axisTick} />
              <ChartTooltip cursor={{ stroke: "var(--border)" }} content={<ChartTooltipContent indicator="line" labelFormatter={tooltipLabel} />} />
              <Area
                type="linear"
                dataKey="successRate"
                stroke="var(--color-successRate)"
                strokeWidth={2}
                fill="url(#fill-success)"
                connectNulls={false}
                dot={loneDot(traffic, "successRate", "var(--color-successRate)")}
                isAnimationActive={false}
              />
            </AreaChart>
          </ChartContainer>
        )}
        <Footnote
          items={[
            { label: "Overall", value: totalRequests ? percent(((totalRequests - totalFailures) / totalRequests) * 100) : "—" },
            { label: "Worst bucket", value: worst ? `${tooltipLabel(worst.time)} · ${percent(worst.successRate)}` : "—" },
          ]}
        />
      </Section>

      <Section
        title="Response time"
        description={`${range.hint} Latency percentiles of successful requests.`}
        actions={
          <Link to="/dashboard/proxies?sort=avg_response_time&order=desc" className="text-muted-foreground hover:text-foreground font-medium">
            Slowest proxies →
          </Link>
        }
      >
        {!hasLatency ? (
          <p className="text-muted-foreground py-8 text-center">No successful requests in this range — nothing to measure yet.</p>
        ) : (
          <ChartContainer config={latencyConfig} className="aspect-auto h-[11rem] w-full">
            <ComposedChart data={traffic} margin={{ left: 0, right: 8, top: 8, bottom: 0 }}>
              {grid}
              {xAxis}
              <YAxis width={48} tickLine={false} axisLine={false} tickFormatter={(v) => `${v} ms`} tick={axisTick} />
              <ChartTooltip cursor={{ stroke: "var(--border)" }} content={<ChartTooltipContent indicator="line" labelFormatter={tooltipLabel} />} />
              <Line type="linear" dataKey="p95" stroke="var(--color-p95)" strokeWidth={2} strokeDasharray="4 3" dot={loneDot(traffic, "p95", "var(--color-p95)")} connectNulls={false} isAnimationActive={false} />
              <Line type="linear" dataKey="p50" stroke="var(--color-p50)" strokeWidth={2} dot={loneDot(traffic, "p50", "var(--color-p50)")} connectNulls={false} isAnimationActive={false} />
            </ComposedChart>
          </ChartContainer>
        )}
        <Footnote
          items={[
            { label: "Peak p50", value: hasLatency ? ms(peakP50) : "—" },
            { label: "Peak p95", value: hasLatency ? ms(peakP95) : "—" },
          ]}
        />
      </Section>

      <DomainStats range={range.value} />
    </>
  )
}
