import * as React from "react"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Section, EmptyLine, LoadingLine } from "@/components/page-header"
import { SearchInput } from "@/components/controls"
import { useResourceQuery } from "@/hooks/use-resource-query"
import { api } from "@/lib/api"
import { bytes, count, percent } from "@/lib/format"
import type { ChartRange } from "@/lib/types"

export function DomainStats({ range }: { range: ChartRange }) {
  const [search, setSearch] = React.useState("")
  const domain = search.trim()
  const query = useResourceQuery(
    ["domain-stats", range, domain],
    () => api.getDomainStats(range, domain),
    { refetchInterval: 30_000 },
  )
  const rows = query.data?.data ?? []

  return (
    <Section
      title="Domains"
      description={`Top 100 hostnames by requests and completed tunnels in the last ${range}. Latency measures successful requests; HTTPS request outcomes appear only with interception enabled.`}
      actions={
        <SearchInput
          value={search}
          onChange={setSearch}
          placeholder="example.com"
          aria-label="Filter domains, including subdomains"
        />
      }
      className="border-b-0"
    >
      {query.isLoading ? (
        <LoadingLine>Loading domain traffic…</LoadingLine>
      ) : query.isError ? (
        <p role="alert" className="text-critical py-8 text-center">
          Failed to load domain traffic.{" "}
          <button type="button" className="text-foreground font-medium underline-offset-4 hover:underline" onClick={() => query.refetch()}>
            Retry
          </button>
        </p>
      ) : rows.length === 0 ? (
        <EmptyLine>{domain ? `No traffic to ${domain} or its subdomains in this range.` : "No domain traffic in this range."}</EmptyLine>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Domain</TableHead>
              <TableHead className="text-right">Requests</TableHead>
              <TableHead className="text-right">Success</TableHead>
              <TableHead className="text-right">Failures</TableHead>
              <TableHead className="text-right">429s</TableHead>
              <TableHead className="text-right">p50 / p95</TableHead>
              <TableHead className="text-right">Tunnels</TableHead>
              <TableHead className="text-right">Tunnel errors</TableHead>
              <TableHead className="text-right">Sent / received</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((row) => (
              <TableRow key={row.domain}>
                <TableCell className="font-mono">{row.domain}</TableCell>
                <TableCell className="num text-right">{count(row.requests)}</TableCell>
                <TableCell className="num text-right">{row.requests ? percent((row.successes / row.requests) * 100) : "—"}</TableCell>
                <TableCell className="num text-right">{count(row.failures)}</TableCell>
                <TableCell className="num text-right">{count(row.rate_limited)}</TableCell>
                <TableCell className="num text-right whitespace-nowrap">
                  {row.successes ? `${count(row.p50_ms)} / ${count(row.p95_ms)} ms` : "—"}
                </TableCell>
                <TableCell className="num text-right">{count(row.tunnels)}</TableCell>
                <TableCell className="num text-right">{count(row.tunnel_errors)}</TableCell>
                <TableCell className="num text-right whitespace-nowrap">
                  {bytes(row.bytes_up)} / {bytes(row.bytes_down)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Section>
  )
}
