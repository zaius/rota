import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { PageHeader, Content, EmptyLine, LoadingLine } from "@/components/page-header"
import { StatStrip } from "@/components/stat-strip"
import { useResourceQuery } from "@/hooks/use-resource-query"
import { api } from "@/lib/api"
import { count, relative } from "@/lib/format"

export default function SessionsPage() {
  const query = useResourceQuery(["sessions"], () => api.getSessions(), { refetchInterval: 5000 })
  const sessions = [...(query.data ?? [])].sort((a, b) => Date.parse(b.last_used) - Date.parse(a.last_used))

  return (
    <>
      <PageHeader
        title="Sessions"
        description="Sticky sessions holding a proxy right now. Each keeps its proxy for one scope until it is released or idles past the session TTL."
      />

      <StatStrip
        columns={3}
        stats={[
          { label: "Live sessions", value: count(sessions.length) },
          { label: "Proxies held", value: count(new Set(sessions.map((s) => s.proxy_id)).size) },
          { label: "Scopes", value: count(new Set(sessions.map((s) => s.scope)).size) },
        ]}
      />

      <Content>
        {query.isLoading ? (
          <LoadingLine>Loading sessions…</LoadingLine>
        ) : query.isError ? (
          <p role="alert" className="text-critical py-8 text-center">
            Failed to load sessions.
          </p>
        ) : sessions.length === 0 ? (
          <EmptyLine>No live sessions.</EmptyLine>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Token</TableHead>
                <TableHead>Scope</TableHead>
                <TableHead className="text-right">Proxy</TableHead>
                <TableHead className="text-right">Last used</TableHead>
                <TableHead className="text-right">Expires</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {sessions.map((s) => (
                <TableRow key={`${s.token}\n${s.scope}`}>
                  <TableCell className="font-mono">{s.token}</TableCell>
                  <TableCell className="font-mono">{s.scope}</TableCell>
                  <TableCell className="num text-right">#{s.proxy_id}</TableCell>
                  <TableCell className="text-right whitespace-nowrap">{relative(s.last_used)}</TableCell>
                  <TableCell className="text-right whitespace-nowrap">{relative(s.expires_at)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Content>
    </>
  )
}
