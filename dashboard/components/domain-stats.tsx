import * as React from "react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { useResourceQuery } from "@/hooks/use-resource-query"
import { api } from "@/lib/api"
import { formatBytes } from "@/lib/format-utils"
import type { ChartRange } from "@/lib/types"

export function DomainStats({ range }: { range: ChartRange }) {
  const [input, setInput] = React.useState("")
  const [domain, setDomain] = React.useState("")
  const query = useResourceQuery(
    ["domain-stats", range, domain],
    () => api.getDomainStats(range, domain),
    { refetchInterval: 30_000 },
  )

  return (
    <Card>
      <CardHeader>
        <CardTitle>Traffic by Domain</CardTitle>
        <CardDescription>
          Top 100 hostnames by requests and completed tunnels in the last {range}.
          Latency measures successful requests. HTTPS request outcomes are visible when interception is enabled.
        </CardDescription>
        <form className="flex gap-2 pt-2" onSubmit={(event) => {
          event.preventDefault()
          setDomain(input.trim())
        }}>
          <Input aria-label="Filter domain" placeholder="example.com (includes subdomains)"
            value={input} onChange={(event) => setInput(event.target.value)} className="max-w-sm" />
          <Button type="submit" variant="outline">Filter</Button>
          {domain && <Button type="button" variant="ghost" onClick={() => {
            setInput("")
            setDomain("")
          }}>Clear</Button>}
        </form>
      </CardHeader>
      <CardContent>
        {query.isLoading ? <p className="text-sm text-muted-foreground">Loading domain traffic…</p> :
          query.isError ? <div role="alert" className="flex items-center gap-3 text-sm text-destructive">
            <span>Failed to load domain traffic.</span>
            <Button variant="outline" size="sm" onClick={() => query.refetch()}>Retry</Button>
          </div> : !query.data?.data.length ?
            <p className="text-sm text-muted-foreground">No domain traffic for this range and filter.</p> :
            <Table>
              <TableHeader><TableRow>
                <TableHead>Domain</TableHead>
                <TableHead className="text-right">Requests</TableHead>
                <TableHead className="text-right">Success</TableHead>
                <TableHead className="text-right">Failures</TableHead>
                <TableHead className="text-right">429s</TableHead>
                <TableHead className="text-right">p50 / p95</TableHead>
                <TableHead className="text-right">Tunnels</TableHead>
                <TableHead className="text-right">Tunnel errors</TableHead>
                <TableHead className="text-right">Sent / Received</TableHead>
              </TableRow></TableHeader>
              <TableBody>{query.data.data.map((row) => (
                <TableRow key={row.domain}>
                  <TableCell className="font-medium">{row.domain}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.requests.toLocaleString()}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.requests ? `${(row.successes / row.requests * 100).toFixed(1)}%` : "—"}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.failures.toLocaleString()}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.rate_limited.toLocaleString()}</TableCell>
                  <TableCell className="text-right tabular-nums whitespace-nowrap">{row.successes ? `${row.p50_ms} / ${row.p95_ms} ms` : "—"}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.tunnels.toLocaleString()}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.tunnel_errors.toLocaleString()}</TableCell>
                  <TableCell className="text-right tabular-nums whitespace-nowrap">{formatBytes(row.bytes_up)} / {formatBytes(row.bytes_down)}</TableCell>
                </TableRow>
              ))}</TableBody>
            </Table>}
      </CardContent>
    </Card>
  )
}
