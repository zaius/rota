import { useState } from "react"
import { Check, ChevronDown, Copy, Eye, EyeOff } from "lucide-react"
import { toast } from "@/lib/toast"
import { api } from "@/lib/api"
import { API_ORIGIN, PROXY_PORT } from "@/lib/config"
import { errorMessage } from "@/lib/utils"
import { type CreateProxyUserRequest, type ProxyUser, type UpdateProxyUserRequest, TLS_PROFILES } from "@/lib/types"
import { useResourceQuery } from "@/hooks/use-resource-query"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogFooter } from "@/components/ui/dialog"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Switch } from "@/components/ui/switch"
import { Label } from "@/components/ui/label"
import { Checkbox } from "@/components/ui/checkbox"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { PageHeader, Content, Section, EmptyLine, LoadingLine } from "@/components/page-header"
import { StatStrip } from "@/components/stat-strip"
import { Tag } from "@/components/status"
import { count } from "@/lib/format"

const DEFAULT_FORM: CreateProxyUserRequest = {
  username: "",
  password: "",
  enabled: true,
  main_pool_id: null,
  fallback_pool_ids: [],
  max_retries: 5,
  inspect_tls: false,
  tls_profile: "go",
  allow_proxy_export: false,
}

// Quotes a value for a POSIX shell command line.
const shellQuote = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`

export default function UsersPage() {
  const usersQuery = useResourceQuery(["proxy-users"], () => api.getProxyUsers().then((r) => r.users))
  const poolsQuery = useResourceQuery(["pools"], () => api.getPools().then((r) => r.pools))
  const users = usersQuery.data ?? []
  const pools = poolsQuery.data ?? []
  const loading = usersQuery.isLoading || poolsQuery.isLoading
  const reload = usersQuery.invalidate

  const [dialogOpen, setDialogOpen] = useState(false)
  const [editUser, setEditUser] = useState<ProxyUser | null>(null)
  const [form, setForm] = useState<CreateProxyUserRequest>(DEFAULT_FORM)
  const [saving, setSaving] = useState(false)
  const [showPass, setShowPass] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<ProxyUser | null>(null)

  // Export command dialog
  const [exportUser, setExportUser] = useState<ProxyUser | null>(null)
  const [exportPool, setExportPool] = useState("")
  const [exportLimit, setExportLimit] = useState("")
  const [exportStatus, setExportStatus] = useState<"active" | "all">("active")
  const [exportFormat, setExportFormat] = useState<"url" | "colon">("url")
  const [copied, setCopied] = useState(false)

  const poolName = (id?: number | null) => {
    if (!id) return "—"
    return pools.find((p) => p.id === id)?.name ?? `Pool #${id}`
  }

  const openCreate = () => {
    setEditUser(null)
    setForm(DEFAULT_FORM)
    setShowPass(false)
    setDialogOpen(true)
  }

  const openEdit = (u: ProxyUser) => {
    setEditUser(u)
    setForm({
      username: u.username,
      password: "",
      enabled: u.enabled,
      main_pool_id: u.main_pool_id ?? null,
      fallback_pool_ids: u.fallback_pool_ids ?? [],
      max_retries: u.max_retries,
      inspect_tls: u.inspect_tls ?? false,
      tls_profile: u.tls_profile || "go",
      allow_proxy_export: u.allow_proxy_export ?? false,
    })
    setShowPass(false)
    setDialogOpen(true)
  }

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!form.username.trim()) {
      toast.error("Username is required")
      return
    }
    if (!editUser && !form.password) {
      toast.error("Password is required")
      return
    }
    setSaving(true)
    try {
      if (editUser) {
        const upd: UpdateProxyUserRequest = {
          enabled: form.enabled,
          main_pool_id: form.main_pool_id,
          fallback_pool_ids: form.fallback_pool_ids,
          max_retries: form.max_retries,
          inspect_tls: form.inspect_tls,
          tls_profile: form.tls_profile,
          allow_proxy_export: form.allow_proxy_export,
        }
        if (form.password) upd.password = form.password
        await api.updateProxyUser(editUser.id, upd)
        toast.success("User updated")
      } else {
        await api.createProxyUser(form)
        toast.success("User created")
      }
      setDialogOpen(false)
      reload()
    } catch (err) {
      toast.error("Failed to save user", errorMessage(err, "Unknown error"))
    } finally {
      setSaving(false)
    }
  }

  const confirmDelete = async () => {
    if (!deleteTarget) return
    try {
      await api.deleteProxyUser(deleteTarget.id)
      toast.success("User deleted")
      reload()
    } catch (err) {
      toast.error("Failed to delete user", errorMessage(err, "Unknown error"))
    } finally {
      setDeleteTarget(null)
    }
  }

  const updateFlag = async (u: ProxyUser, patch: UpdateProxyUserRequest) => {
    try {
      await api.updateProxyUser(u.id, patch)
      reload()
    } catch (err) {
      toast.error("Failed to update user", errorMessage(err, "Unknown error"))
    }
  }

  const toggleFallback = (poolId: number) => {
    const current = form.fallback_pool_ids ?? []
    setForm({
      ...form,
      fallback_pool_ids: current.includes(poolId) ? current.filter((x) => x !== poolId) : [...current, poolId],
    })
  }

  const copyProxyURL = (u: ProxyUser) => {
    const host = window.location.hostname
    navigator.clipboard.writeText(`http://${u.username}:***@${host}:${PROXY_PORT}`)
    toast.success("Proxy URL copied", "Replace *** with the user's password")
  }

  // ── Export command ────────────────────────────────────────────────────────

  // A proxy user may export any pool in its chain; the endpoint picks the main
  // pool when the command names none.
  const exportPools = (u: ProxyUser) =>
    [u.main_pool_id, ...(u.fallback_pool_ids ?? [])].filter((id): id is number => !!id && pools.some((p) => p.id === id))

  const openExport = (u: ProxyUser) => {
    setExportUser(u)
    setExportPool(u.main_pool_id ? "" : String(exportPools(u)[0] ?? ""))
    setExportLimit("")
    setExportStatus("active")
    setExportFormat("url")
    setCopied(false)
  }

  // The password travels as HTTP Basic credentials, never in the URL, so it
  // stays out of server logs and browser history.
  const exportCommand = (u: ProxyUser) => {
    const params = new URLSearchParams()
    if (exportPool) params.set("pool", exportPool)
    if (parseInt(exportLimit) > 0) params.set("limit", String(parseInt(exportLimit)))
    if (exportStatus !== "active") params.set("status", exportStatus)
    if (exportFormat !== "url") params.set("format", exportFormat)
    const query = params.toString()
    const url = `${API_ORIGIN()}/api/v1/proxies/working${query ? `?${query}` : ""}`
    return `curl -u ${shellQuote(`${u.username}:YOUR_PASSWORD`)} ${shellQuote(url)}`
  }

  const enabled = users.filter((u) => u.enabled).length
  const withPool = users.filter((u) => u.main_pool_id).length
  const inspecting = users.filter((u) => u.inspect_tls).length

  return (
    <>
      <PageHeader
        title="Users"
        description={
          <>
            Credentials clients use on port <span className="font-mono">{PROXY_PORT}</span>. A user is routed through its main pool, then its
            fallbacks in order; without a pool it rotates over the whole inventory.
          </>
        }
      >
        <Button onClick={openCreate}>Add user</Button>
      </PageHeader>

      <StatStrip
        columns={4}
        stats={[
          { label: "Users", value: count(users.length), hint: `${count(enabled)} enabled` },
          { label: "With a main pool", value: count(withPool), hint: `${count(users.length - withPool)} on global rotation` },
          { label: "Inspecting HTTPS", value: count(inspecting), hint: "requests inside tunnels are recorded" },
          {
            label: "Export allowed",
            value: count(users.filter((u) => u.allow_proxy_export).length),
            hint: "can download their pools' proxies",
          },
        ]}
      />

      <Content>
        {loading ? (
          <LoadingLine />
        ) : users.length === 0 ? (
          <EmptyLine>No users yet. Clients cannot authenticate to the proxy until one exists.</EmptyLine>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Username</TableHead>
                <TableHead>Main pool</TableHead>
                <TableHead>Fallbacks</TableHead>
                <TableHead className="text-right">Max retries</TableHead>
                <TableHead>HTTPS</TableHead>
                <TableHead>Enabled</TableHead>
                <TableHead>Export</TableHead>
                <TableHead className="w-8" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {users.map((u) => (
                <TableRow key={u.id}>
                  <TableCell className="font-mono font-medium">{u.username}</TableCell>
                  <TableCell>
                    {u.main_pool_id ? (
                      <Tag strong>{u.main_pool_name || poolName(u.main_pool_id)}</Tag>
                    ) : (
                      <span className="text-muted-foreground">global rotation</span>
                    )}
                  </TableCell>
                  <TableCell>
                    {u.fallback_pool_ids && u.fallback_pool_ids.length > 0 ? (
                      <span className="flex flex-wrap gap-1">
                        {u.fallback_pool_ids.map((id, i) => (
                          <Tag key={id}>
                            {i + 1}. {poolName(id)}
                          </Tag>
                        ))}
                      </span>
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  <TableCell className="num text-right">{u.max_retries}</TableCell>
                  <TableCell className="text-muted-foreground">
                    {u.inspect_tls ? (
                      <span title="Rota terminates TLS and records each request">
                        inspected
                        {u.tls_profile && u.tls_profile !== "go" && <span className="ml-1.5 font-mono">· {u.tls_profile}</span>}
                      </span>
                    ) : (
                      "tunnelled"
                    )}
                  </TableCell>
                  <TableCell>
                    <Switch checked={u.enabled} onCheckedChange={() => updateFlag(u, { enabled: !u.enabled })} aria-label={`${u.username} enabled`} />
                  </TableCell>
                  <TableCell>
                    <Switch
                      checked={u.allow_proxy_export ?? false}
                      onCheckedChange={() => updateFlag(u, { allow_proxy_export: !u.allow_proxy_export })}
                      aria-label={`${u.username} may export proxies`}
                    />
                  </TableCell>
                  <TableCell className="text-right">
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${u.username}`}>
                          <ChevronDown aria-hidden />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => copyProxyURL(u)}>Copy proxy URL</DropdownMenuItem>
                        {u.allow_proxy_export && <DropdownMenuItem onClick={() => openExport(u)}>Export command…</DropdownMenuItem>}
                        <DropdownMenuItem onClick={() => openEdit(u)}>Edit</DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem variant="destructive" onClick={() => setDeleteTarget(u)}>
                          Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Content>

      <Section title="How routing works" className="border-b-0">
        <dl className="grid gap-x-8 gap-y-4 sm:grid-cols-3">
          <div>
            <dt className="label">Authentication</dt>
            <dd className="mt-0.5">
              Clients send Proxy-Authorization, as in <span className="font-mono whitespace-nowrap">http://user:pass@host:{PROXY_PORT}</span>.
            </dd>
          </div>
          <div>
            <dt className="label">Pool chain</dt>
            <dd className="mt-0.5">
              Main pool first; when it has no alive proxy, requests cascade to fallbacks in order. Each pool keeps its own rotation.
            </dd>
          </div>
          <div>
            <dt className="label">Retries</dt>
            <dd className="mt-0.5">
              Each retry picks a different proxy across the chain, skipping ones that already failed within the same request.
            </dd>
          </div>
        </dl>
      </Section>

      {/* Add / edit */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-[30rem]">
          <form onSubmit={handleSave} className="space-y-4">
            <DialogHeader>
              <DialogTitle>{editUser ? `Edit ${editUser.username}` : "Add user"}</DialogTitle>
              <DialogDescription>
                {editUser
                  ? "Open connections keep their current proxy; new requests use the updated chain."
                  : "The user can authenticate as soon as it is saved."}
              </DialogDescription>
            </DialogHeader>

            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="user-name">Username</Label>
                <Input
                  id="user-name"
                  className="font-mono"
                  required
                  value={form.username}
                  onChange={(e) => setForm({ ...form, username: e.target.value })}
                  disabled={!!editUser}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="user-pass">{editUser ? "New password" : "Password"}</Label>
                <div className="relative">
                  <Input
                    id="user-pass"
                    type={showPass ? "text" : "password"}
                    autoComplete="new-password"
                    placeholder={editUser ? "leave blank to keep" : "min 6 characters"}
                    value={form.password}
                    onChange={(e) => setForm({ ...form, password: e.target.value })}
                    className="pr-8"
                  />
                  <button
                    type="button"
                    className="text-muted-foreground hover:text-foreground absolute top-1/2 right-2 -translate-y-1/2"
                    onClick={() => setShowPass((v) => !v)}
                    aria-label={showPass ? "Hide password" : "Show password"}
                  >
                    {showPass ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
                  </button>
                </div>
              </div>
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="user-pool">Main pool</Label>
              <Select
                value={form.main_pool_id?.toString() ?? "none"}
                onValueChange={(v) => setForm({ ...form, main_pool_id: v === "none" ? null : parseInt(v) })}
              >
                <SelectTrigger id="user-pool" className="w-full">
                  <SelectValue placeholder="Select main pool" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">No pool — global rotation</SelectItem>
                  {pools.map((p) => (
                    <SelectItem key={p.id} value={p.id.toString()}>
                      {p.name}{" "}
                      <span className="text-muted-foreground">
                        ({p.active_proxies}/{p.total_proxies} active)
                      </span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="space-y-1.5">
              <Label>Fallback pools, in priority order</Label>
              {pools.length === 0 ? (
                <p className="text-muted-foreground">No pools yet.</p>
              ) : (
                <div className="border-border divide-border max-h-44 divide-y overflow-y-auto rounded-md border">
                  {pools
                    .filter((p) => p.id !== form.main_pool_id)
                    .map((p) => {
                      const checked = (form.fallback_pool_ids ?? []).includes(p.id)
                      const idx = (form.fallback_pool_ids ?? []).indexOf(p.id)
                      return (
                        <label key={p.id} className="hover:bg-accent/50 flex cursor-pointer items-center gap-3 px-2.5 py-1.5">
                          <Checkbox checked={checked} onCheckedChange={() => toggleFallback(p.id)} />
                          <span className="min-w-0 flex-1 truncate font-medium">{p.name}</span>
                          <span className="text-muted-foreground num">
                            {p.active_proxies} active · {p.rotation_method}
                          </span>
                          {checked && <Tag strong>#{idx + 1}</Tag>}
                        </label>
                      )
                    })}
                </div>
              )}
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="user-retries">Max retries across the chain</Label>
              <Input
                id="user-retries"
                type="number"
                min={1}
                max={50}
                className="w-28"
                value={form.max_retries}
                onChange={(e) => setForm({ ...form, max_retries: parseInt(e.target.value) || 5 })}
              />
              <p className="text-muted-foreground text-[0.6875rem] leading-4">
                Each retry picks a different proxy; ones that failed are skipped for the rest of the request.
              </p>
            </div>

            <div className="border-border divide-border divide-y border-y">
              <div className="space-y-2.5 py-2.5">
                <div className="flex items-start justify-between gap-6">
                  <div>
                    <Label htmlFor="user-inspect-tls" className="text-foreground cursor-pointer">
                      Inspect HTTPS
                    </Label>
                    <p className="text-muted-foreground mt-0.5 text-[0.6875rem] leading-4">
                      Off, a CONNECT tunnel is opaque and counts as one event however many requests pass through it. On, Rota terminates TLS
                      to record each request; clients must trust the server CA, and connections drop to HTTP/1.1.
                    </p>
                  </div>
                  <Switch id="user-inspect-tls" checked={form.inspect_tls ?? false} onCheckedChange={(v) => setForm({ ...form, inspect_tls: v })} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="user-tls-profile">TLS fingerprint</Label>
                  <Select
                    value={form.tls_profile || "go"}
                    onValueChange={(v) => setForm({ ...form, tls_profile: v as CreateProxyUserRequest["tls_profile"] })}
                    disabled={!form.inspect_tls}
                  >
                    <SelectTrigger id="user-tls-profile" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {TLS_PROFILES.map((p) => (
                        <SelectItem key={p.value} value={p.value}>
                          {p.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <p className="text-muted-foreground text-[0.6875rem] leading-4">
                    Applies only while inspecting: Rota then presents this client&apos;s ClientHello and HTTP/2 settings to the target instead of
                    Go&apos;s. Without interception the client makes its own handshake.
                  </p>
                </div>
              </div>
              <div className="flex items-start justify-between gap-6 py-2.5">
                <div>
                  <Label htmlFor="user-allow-export" className="text-foreground cursor-pointer">
                    Allow proxy export
                  </Label>
                  <p className="text-muted-foreground mt-0.5 text-[0.6875rem] leading-4">
                    Lets this user download the proxies of its own pools, upstream credentials included, from{" "}
                    <span className="font-mono">GET /api/v1/proxies/working</span> with its proxy credentials — for clients that connect to
                    proxies directly instead of through Rota.
                  </p>
                </div>
                <Switch
                  id="user-allow-export"
                  checked={form.allow_proxy_export ?? false}
                  onCheckedChange={(v) => setForm({ ...form, allow_proxy_export: v })}
                />
              </div>
              <div className="flex items-center justify-between gap-6 py-2.5">
                <Label htmlFor="user-enabled" className="text-foreground cursor-pointer">
                  Enabled
                </Label>
                <Switch id="user-enabled" checked={form.enabled} onCheckedChange={(v) => setForm({ ...form, enabled: v })} />
              </div>
            </div>

            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setDialogOpen(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={saving}>
                {saving ? "Saving…" : editUser ? "Save changes" : "Create user"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Export command */}
      <Dialog open={!!exportUser} onOpenChange={(open) => !open && setExportUser(null)}>
        <DialogContent className="sm:max-w-[34rem]">
          <DialogHeader>
            <DialogTitle>Export command for {exportUser?.username}</DialogTitle>
            <DialogDescription>
              Downloads the pool&apos;s proxies, one per line with upstream credentials. The user&apos;s password goes in the Authorization header,
              never the URL.
            </DialogDescription>
          </DialogHeader>
          {exportUser &&
            (exportPools(exportUser).length === 0 ? (
              <p className="text-muted-foreground">This user has no pools to export. Give it a main or fallback pool first.</p>
            ) : (
              <div className="space-y-4">
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <Label htmlFor="export-pool">Pool</Label>
                    <Select value={exportPool || "main"} onValueChange={(v) => setExportPool(v === "main" ? "" : v)}>
                      <SelectTrigger id="export-pool" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {exportUser.main_pool_id && <SelectItem value="main">Main pool ({poolName(exportUser.main_pool_id)})</SelectItem>}
                        {exportPools(exportUser)
                          .filter((id) => id !== exportUser.main_pool_id)
                          .map((id) => (
                            <SelectItem key={id} value={String(id)}>
                              {poolName(id)}
                            </SelectItem>
                          ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="export-status">Proxies</Label>
                    <Select value={exportStatus} onValueChange={(v) => setExportStatus(v as "active" | "all")}>
                      <SelectTrigger id="export-status" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="active">Active, outside cooldown</SelectItem>
                        <SelectItem value="all">Every member</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="export-limit">Limit (optional)</Label>
                    <Input id="export-limit" type="number" min={1} placeholder="all" value={exportLimit} onChange={(e) => setExportLimit(e.target.value)} />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="export-format">Line format</Label>
                    <Select value={exportFormat} onValueChange={(v) => setExportFormat(v as "url" | "colon")}>
                      <SelectTrigger id="export-format" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="url">protocol://user:pass@host:port</SelectItem>
                        <SelectItem value="colon">host:port:user:pass</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="export-command">Command</Label>
                  <div className="flex items-start gap-2">
                    <pre
                      id="export-command"
                      className="border-border bg-muted/40 min-w-0 flex-1 rounded-md border px-2.5 py-2 font-mono text-[0.75rem] leading-5 break-all whitespace-pre-wrap select-all"
                    >
                      {exportCommand(exportUser)}
                    </pre>
                    <Button
                      type="button"
                      variant="outline"
                      className="shrink-0"
                      onClick={() => {
                        navigator.clipboard.writeText(exportCommand(exportUser))
                        setCopied(true)
                        setTimeout(() => setCopied(false), 2000)
                      }}
                    >
                      {copied ? <Check aria-hidden /> : <Copy aria-hidden />}
                      {copied ? "Copied" : "Copy"}
                    </Button>
                  </div>
                  <p className="text-muted-foreground text-[0.6875rem] leading-4">
                    Replace <span className="font-mono">YOUR_PASSWORD</span> with the user&apos;s proxy password.
                  </p>
                </div>
              </div>
            ))}
          <DialogFooter>
            <Button variant="outline" onClick={() => setExportUser(null)}>
              Close
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete user “{deleteTarget?.username}”?</AlertDialogTitle>
            <AlertDialogDescription>Clients using these credentials get 407 on their next request. Pools are not affected.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={confirmDelete}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
