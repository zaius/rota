
import { useEffect, useState, useCallback, useRef } from "react"
import {
  Plus, Trash2, RefreshCw,
  Pencil, Loader2, Layers, ShieldCheck, Globe,
  Download, Bell, BellOff, Tag, X,
} from "lucide-react"
import { toast } from "sonner"
import { api } from "@/lib/api"
import { errorMessage } from "@/lib/utils"
import {
  ProxyPool, PoolProxy, Job, CreatePoolRequest, GeoFilter,
  PoolAlertRule, CreatePoolAlertRuleRequest, GEO_FILTER_ALL,
} from "@/lib/types"
import { useResourceQuery } from "@/hooks/use-resource-query"
import { useUrlState } from "@/hooks/use-url-state"
import { EmptyState } from "@/components/crud/empty-state"
import { PageSpinner } from "@/components/crud/page-spinner"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter,
} from "@/components/ui/dialog"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table"
import { Switch } from "@/components/ui/switch"
import { Label } from "@/components/ui/label"
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card"
import { Progress } from "@/components/ui/progress"
import { Checkbox } from "@/components/ui/checkbox"
import { GeoSelector } from "@/components/geo-selector"
import { TagInput } from "@/components/tag-input"

// ────────────────────────────────────────────────────────────────────────────
// Types & helpers
// ────────────────────────────────────────────────────────────────────────────

const ROTATION_LABELS: Record<string, string> = {
  roundrobin: "Round Robin",
  random: "Random",
  stick: "Sticky (N requests)",
  session: "Session (sticky until released/idle)",
}

const FLAG_CDN = (cc: string) =>
  `https://flagcdn.com/16x12/${cc.toLowerCase()}.png`

const hasAllCountries = (filters?: GeoFilter[]) =>
  (filters ?? []).some(f => f.country_code === GEO_FILTER_ALL)

const geoFilterKey = (f: GeoFilter) => `${f.country_code}-${f.city_name ?? ""}`

const statusColor = (s: string) =>
  s === "active" ? "text-green-500" : s === "failed" ? "text-red-500" : "text-yellow-500"

const DEFAULT_POOL_FORM: CreatePoolRequest = {
  name: "",
  description: "",
  country_code: undefined,
  region_name: undefined,
  city_name: undefined,
  rotation_method: "roundrobin",
  stick_count: 10,
  session_ttl_minutes: 10,
  health_check_url: "https://api.ipify.org",
  health_check_cron: "*/30 * * * *",
  health_check_enabled: true,
  auto_sync: true,
  sync_mode: "auto",
  enabled: true,
  isp_filters: [],
  tag_filters: [],
}

// The open tab and selected pool, kept in the URL.
const URL_DEFAULTS = { tab: "pools", pool: "" }

// ────────────────────────────────────────────────────────────────────────────
// Main page
// ────────────────────────────────────────────────────────────────────────────

export default function PoolsPage() {
  const poolsQuery = useResourceQuery(["pools"], () => api.getPools().then(r => r.pools))
  const geoQuery = useResourceQuery(["geo-countries"], () => api.getGeoByCountry().then(r => r.geo))
  const pools = poolsQuery.data ?? []
  const geoCountries = geoQuery.data ?? []
  const loading = poolsQuery.isLoading || geoQuery.isLoading
  const loadAll = () => { poolsQuery.invalidate(); geoQuery.invalidate() }
  const [url, setUrl] = useUrlState(URL_DEFAULTS)
  const activeTab = url.tab === "geo" ? "geo" : "pools"
  const setActiveTab = (tab: "pools" | "geo") => setUrl({ tab })


  // Pool detail panel
  // The page reads the selected pool from the live list, so edits and syncs
  // show up in the detail panel without reselecting it.
  const selectedPoolId = Number(url.pool) || 0
  const selectedPool = pools.find(p => p.id === selectedPoolId) ?? null
  const [poolProxies, setPoolProxies] = useState<PoolProxy[]>([])
  const [poolProxiesLoading, setPoolProxiesLoading] = useState(false)
  // Identifies the most recent pool selection, so a slow response for a pool
  // the user has already navigated away from is discarded instead of
  // overwriting the current one.
  const selectedPoolReq = useRef(0)
  const [hcJob, setHcJob] = useState<Job | null>(null)
  const [hcRunning, setHcRunning] = useState(false)
  const [syncing, setSyncing] = useState(false)
  const hcPollRef = useRef<ReturnType<typeof setInterval> | null>(null)

  // Dialog
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editPool, setEditPool] = useState<ProxyPool | null>(null)
  const [form, setForm] = useState<CreatePoolRequest>(DEFAULT_POOL_FORM)
  const [saving, setSaving] = useState(false)
  const formAllCountries = hasAllCountries(form.geo_filters)
  const formGeoCount = form.geo_filters?.length ?? 0

  // Quick-add geo filter inside edit dialog
  const [newGeoCountry, setNewGeoCountry] = useState("")
  const [newGeoCity, setNewGeoCity] = useState("")

  // Suggestions for the tag and ISP filter inputs (best-effort)
  const tagListQuery = useResourceQuery(["proxy-tags"], () => api.getTagList())
  const ispListQuery = useResourceQuery(["isp-list"], () => api.getISPList())

  // Manual "add proxies to pool" picker
  const [pickerOpen, setPickerOpen] = useState(false)
  const [pickerSearch, setPickerSearch] = useState("")
  const [pickerQuery, setPickerQuery] = useState("")
  const [pickerSelected, setPickerSelected] = useState<number[]>([])
  const [addingProxies, setAddingProxies] = useState(false)
  useEffect(() => {
    const timer = setTimeout(() => setPickerQuery(pickerSearch.trim()), 300)
    return () => clearTimeout(timer)
  }, [pickerSearch])
  const pickerResults = useResourceQuery(
    ["pool-picker", pickerQuery],
    () => api.getProxies({ page: 1, limit: 50, search: pickerQuery || undefined }).then(r => r.proxies),
    { enabled: pickerOpen },
  )

  // Alert rules
  const [alertRules, setAlertRules] = useState<PoolAlertRule[]>([])
  const [alertDialogOpen, setAlertDialogOpen] = useState(false)
  const [alertForm, setAlertForm] = useState<CreatePoolAlertRuleRequest>({
    enabled: true, min_active_proxies: 5, webhook_url: "", webhook_method: "POST", cooldown_minutes: 30,
  })
  const [editAlertRule, setEditAlertRule] = useState<PoolAlertRule | null>(null)
  const [savingAlert, setSavingAlert] = useState(false)


  const openCreate = () => {
    setEditPool(null)
    setForm({ ...DEFAULT_POOL_FORM, geo_filters: [] })
    setNewGeoCountry("")
    setNewGeoCity("")
    tagListQuery.invalidate()
    ispListQuery.invalidate()
    setDialogOpen(true)
  }

  const openEdit = (p: ProxyPool) => {
    setEditPool(p)
    setForm({
      name: p.name,
      description: p.description,
      country_code: p.country_code,
      region_name: p.region_name,
      city_name: p.city_name,
      rotation_method: p.rotation_method,
      stick_count: p.stick_count,
      session_ttl_minutes: p.session_ttl_minutes ?? 10,
      health_check_url: p.health_check_url,
      health_check_cron: p.health_check_cron,
      health_check_enabled: p.health_check_enabled,
      auto_sync: p.auto_sync,
      sync_mode: p.sync_mode ?? "auto",
      enabled: p.enabled,
      geo_filters: p.geo_filters ?? [],
      isp_filters: p.isp_filters ?? [],
      tag_filters: p.tag_filters ?? [],
    })
    setNewGeoCountry("")
    setNewGeoCity("")
    tagListQuery.invalidate()
    ispListQuery.invalidate()
    setDialogOpen(true)
  }

  const handleSave = async () => {
    if (!form.name.trim()) { toast.error("Name is required"); return }
    setSaving(true)
    try {
      if (editPool) {
        await api.updatePool(editPool.id, form)
        toast.success("Pool updated")
      } else {
        await api.createPool(form)
        toast.success("Pool created")
      }
      setDialogOpen(false)
      loadAll()
    } catch (e) {
      toast.error(errorMessage(e, "Failed to save pool"))
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async (id: number) => {
    if (!confirm("Delete this pool?")) return
    try {
      await api.deletePool(id)
      toast.success("Pool deleted")
      if (selectedPoolId === id) setUrl({ pool: "" })
      loadAll()
    } catch {
      toast.error("Failed to delete pool")
    }
  }

  const handleSelectPool = (pool: ProxyPool) => {
    if (pool.id !== selectedPoolId) setUrl({ pool: String(pool.id) })
  }

  // loadPoolDetail fetches the members and alert rules of one pool.
  const loadPoolDetail = useCallback(async (poolId: number) => {
    const reqId = ++selectedPoolReq.current
    setPoolProxiesLoading(true)
    try {
      const [proxiesRes, rules] = await Promise.all([
        api.getPoolProxies(poolId),
        api.getAlertRules(poolId).catch(() => []),
      ])
      if (selectedPoolReq.current !== reqId) return
      setPoolProxies(proxiesRes.proxies)
      setAlertRules(rules)
    } catch {
      if (selectedPoolReq.current !== reqId) return
      toast.error("Failed to load pool proxies")
    } finally {
      if (selectedPoolReq.current === reqId) setPoolProxiesLoading(false)
    }
  }, [])

  // A running check's progress belongs to the pool that started it, so a new
  // selection stops following it.
  const selectedPoolIdRef = useRef(selectedPoolId)
  useEffect(() => {
    selectedPoolIdRef.current = selectedPoolId
    if (hcPollRef.current) {
      clearInterval(hcPollRef.current)
      hcPollRef.current = null
    }
    setHcRunning(false)
    setHcJob(null)
    if (selectedPoolId) {
      loadPoolDetail(selectedPoolId)
    } else {
      setPoolProxies([])
      setAlertRules([])
    }
  }, [selectedPoolId, loadPoolDetail])

  const handleExport = async (format: "txt" | "csv") => {
    if (!selectedPool) return
    try {
      const blob = await api.exportPool(selectedPool.id, format)
      const url = URL.createObjectURL(blob)
      const a = document.createElement("a")
      a.href = url
      a.download = `${selectedPool.name}.${format}`
      a.click()
      URL.revokeObjectURL(url)
    } catch (error) {
      console.error("Failed to export pool:", error)
      toast.error(error instanceof Error ? error.message : "Failed to export pool")
    }
  }

  const openCreateAlertRule = () => {
    setEditAlertRule(null)
    setAlertForm({ enabled: true, min_active_proxies: 5, webhook_url: "", webhook_method: "POST", cooldown_minutes: 30 })
    setAlertDialogOpen(true)
  }

  const openEditAlertRule = (rule: PoolAlertRule) => {
    setEditAlertRule(rule)
    setAlertForm({
      enabled: rule.enabled,
      min_active_proxies: rule.min_active_proxies,
      webhook_url: rule.webhook_url,
      webhook_method: rule.webhook_method,
      cooldown_minutes: rule.cooldown_minutes,
    })
    setAlertDialogOpen(true)
  }

  const handleSaveAlertRule = async () => {
    if (!selectedPool) return
    setSavingAlert(true)
    try {
      if (editAlertRule) {
        await api.updateAlertRule(selectedPool.id, editAlertRule.id, alertForm)
        toast.success("Alert rule updated")
      } else {
        await api.createAlertRule(selectedPool.id, alertForm)
        toast.success("Alert rule created")
      }
      setAlertDialogOpen(false)
      const rules = await api.getAlertRules(selectedPool.id)
      setAlertRules(rules)
    } catch {
      toast.error("Failed to save alert rule")
    } finally {
      setSavingAlert(false)
    }
  }

  const handleDeleteAlertRule = async (ruleId: number) => {
    if (!selectedPool || !confirm("Delete this alert rule?")) return
    try {
      await api.deleteAlertRule(selectedPool.id, ruleId)
      setAlertRules(prev => prev.filter(r => r.id !== ruleId))
      toast.success("Alert rule deleted")
    } catch {
      toast.error("Failed to delete alert rule")
    }
  }

  const handleSync = async () => {
    if (!selectedPool) return
    setSyncing(true)
    try {
      const res = await api.syncPool(selectedPool.id)
      toast.success(`Synced ${res.synced} proxies into pool`)
      loadPoolDetail(selectedPool.id)
      loadAll()
    } catch {
      toast.error("Sync failed")
    } finally {
      setSyncing(false)
    }
  }

  // ── Manual pool membership ────────────────────────────────────────────────

  const openPicker = () => {
    setPickerSearch("")
    setPickerQuery("")
    setPickerSelected([])
    setPickerOpen(true)
  }

  const handleAddProxiesToPool = async () => {
    if (!selectedPool || pickerSelected.length === 0) return
    setAddingProxies(true)
    try {
      const res = await api.addPoolProxies(selectedPool.id, pickerSelected)
      toast.success(`Added ${res.added} ${res.added === 1 ? "proxy" : "proxies"} to the pool`)
      setPickerOpen(false)
      loadPoolDetail(selectedPool.id)
      loadAll()
    } catch {
      toast.error("Failed to add proxies to the pool")
    } finally {
      setAddingProxies(false)
    }
  }

  const handleRemoveProxyFromPool = async (proxyId: number) => {
    if (!selectedPool) return
    try {
      await api.removePoolProxies(selectedPool.id, [proxyId])
      setPoolProxies(prev => prev.filter(p => p.proxy_id !== proxyId))
      toast.success("Proxy removed from the pool")
      loadAll()
    } catch {
      toast.error("Failed to remove proxy from the pool")
    }
  }

  // A pool whose filters rebuild its membership on sync drops manual
  // additions that don't match them.
  const rebuildsFromFilters = (p: ProxyPool) =>
    p.sync_mode !== "manual" &&
    ((p.geo_filters?.length ?? 0) > 0 || (p.isp_filters?.length ?? 0) > 0 ||
      (p.tag_filters?.length ?? 0) > 0 || !!p.country_code)

  const stopHcPoll = useCallback(() => {
    if (hcPollRef.current) {
      clearInterval(hcPollRef.current)
      hcPollRef.current = null
    }
  }, [])

  const handleHealthCheck = async () => {
    if (!selectedPool) return
    setHcRunning(true)
    setHcJob(null)
    stopHcPoll()
    try {
      const poolId = selectedPool.id
      const res = await api.healthCheckPool(poolId, selectedPool.health_check_url, 20)
      if (selectedPoolIdRef.current !== poolId) return
      // Start polling job status
      hcPollRef.current = setInterval(async () => {
        try {
          const job = await api.getHealthCheckJob(poolId, res.job_id)
          if (selectedPoolIdRef.current !== poolId) return
          setHcJob(job)
          if (job.status === "done" || job.status === "failed") {
            stopHcPoll()
            setHcRunning(false)
            if (job.status === "done") {
              toast.success(`Health check done: ${job.active}/${job.progress} active`)
            } else {
              toast.error(`Health check failed: ${job.error}`)
            }
            loadPoolDetail(poolId)
            loadAll()
          }
        } catch {
          stopHcPoll()
          setHcRunning(false)
        }
      }, 1500)
    } catch {
      toast.error("Failed to start health check")
      setHcRunning(false)
    }
  }

  // Cleanup on unmount
  useEffect(() => () => stopHcPoll(), [stopHcPoll])

  if (loading) {
    return <PageSpinner />
  }

  return (
    <div className="flex flex-col gap-6 p-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Proxy Pools</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Named groups of proxies with geo, ISP or tag filters and independent rotation strategies
          </p>
        </div>
        <Button size="sm" onClick={openCreate}>
          <Plus className="h-4 w-4 mr-2" />
          New Pool
        </Button>
      </div>

      {/* Custom tab bar */}
      <div className="flex gap-1 border-b pb-0">
        <button
          className={`flex items-center gap-2 px-4 py-2 text-sm font-medium border-b-2 transition-colors ${activeTab === "pools" ? "border-primary text-primary" : "border-transparent text-muted-foreground hover:text-foreground"}`}
          onClick={() => setActiveTab("pools")}
        >
          <Layers className="h-4 w-4" />Pools
        </button>
        <button
          className={`flex items-center gap-2 px-4 py-2 text-sm font-medium border-b-2 transition-colors ${activeTab === "geo" ? "border-primary text-primary" : "border-transparent text-muted-foreground hover:text-foreground"}`}
          onClick={() => setActiveTab("geo")}
        >
          <Globe className="h-4 w-4" />Geo Distribution
        </button>
      </div>

        {/* ── Pools tab ─────────���─────────────────────────────��───────────── */}
        {activeTab === "pools" && <div className="mt-4">
          {pools.length === 0 ? (
            <EmptyState
              icon={Layers}
              message="No pools yet. Create one to get started."
              action={<Button onClick={openCreate}><Plus className="h-4 w-4 mr-2" />New Pool</Button>}
            />
          ) : (
            <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
              {/* Pool list */}
              <div className="flex flex-col gap-2">
                {pools.map(pool => (
                  <Card
                    key={pool.id}
                    className={`cursor-pointer transition-colors hover:border-primary/60 ${selectedPool?.id === pool.id ? "border-primary" : ""}`}
                    onClick={() => handleSelectPool(pool)}
                  >
                    <CardContent className="p-4">
                      <div className="flex items-start justify-between gap-2">
                        <div className="flex-1 min-w-0">
                          <div className="flex items-center gap-2 flex-wrap">
                            <span className="font-semibold truncate">{pool.name}</span>
                            {!pool.enabled && <Badge variant="secondary">Disabled</Badge>}
                            <Badge variant="outline" className="text-xs">
                              {ROTATION_LABELS[pool.rotation_method] || pool.rotation_method}
                            </Badge>
                          </div>
                          {pool.description && (
                            <p className="text-xs text-muted-foreground mt-0.5 truncate">{pool.description}</p>
                          )}
                          <div className="flex items-center gap-2 mt-2 text-xs text-muted-foreground flex-wrap">
                            {hasAllCountries(pool.geo_filters) ? (
                              <span className="flex items-center gap-1">
                                <Globe className="h-3 w-3" /> All countries
                              </span>
                            ) : (pool.geo_filters?.length ?? 0) > 0 ? (
                              <>
                                {pool.geo_filters!.slice(0, 8).map(f => (
                                  <span key={geoFilterKey(f)} className="flex items-center gap-1">
                                    <img src={FLAG_CDN(f.country_code)} alt={f.country_code} className="h-3" />
                                    {f.country_code}{f.city_name ? `/${f.city_name}` : ""}
                                  </span>
                                ))}
                                {pool.geo_filters!.length > 8 && <span>+{pool.geo_filters!.length - 8}</span>}
                              </>
                            ) : (
                              <>
                                {pool.country_code && (
                                  <span className="flex items-center gap-1">
                                    <img src={FLAG_CDN(pool.country_code)} alt={pool.country_code} className="h-3" />
                                    {pool.country_code}
                                  </span>
                                )}
                                {pool.region_name && <span>{pool.region_name}</span>}
                                {pool.city_name && <span>{pool.city_name}</span>}
                              </>
                            )}
                          </div>
                        </div>
                        <div className="flex flex-col items-end gap-1 text-xs shrink-0">
                          <span className="font-bold text-base">{pool.total_proxies}</span>
                          <span className="text-muted-foreground">total</span>
                          <div className="flex gap-1">
                            <span className="text-green-500">{pool.active_proxies} ✓</span>
                            <span className="text-red-500">{pool.failed_proxies} ✗</span>
                          </div>
                        </div>
                      </div>
                      {pool.total_proxies > 0 && (
                        <Progress
                          className="h-1 mt-2"
                          value={(pool.active_proxies / pool.total_proxies) * 100}
                        />
                      )}
                    </CardContent>
                  </Card>
                ))}
              </div>

              {/* Pool detail */}
              {selectedPool ? (
                <div className="flex flex-col gap-3">
                  <Card>
                    <CardHeader className="pb-3">
                      <div className="flex items-center justify-between">
                        <CardTitle className="text-base">{selectedPool.name}</CardTitle>
                        <div className="flex gap-1">
                          <Button
                            variant="outline" size="sm"
                            onClick={handleSync}
                            disabled={syncing}
                            title="Re-sync proxies from pool filters"
                          >
                            {syncing
                              ? <Loader2 className="h-3 w-3 animate-spin mr-1" />
                              : <RefreshCw className="h-3 w-3 mr-1" />}
                            Sync
                          </Button>
                          <Button
                            variant="outline" size="sm"
                            onClick={handleHealthCheck}
                            disabled={hcRunning}
                            title="Run health check on all proxies in pool"
                          >
                            {hcRunning
                              ? <Loader2 className="h-3 w-3 animate-spin mr-1" />
                              : <ShieldCheck className="h-3 w-3 mr-1" />}
                            Check
                          </Button>
                          <Button
                            variant="outline" size="sm"
                            onClick={() => handleExport("txt")}
                            title="Export as TXT"
                          >
                            <Download className="h-3 w-3 mr-1" />
                            TXT
                          </Button>
                          <Button
                            variant="outline" size="sm"
                            onClick={() => handleExport("csv")}
                            title="Export as CSV"
                          >
                            <Download className="h-3 w-3 mr-1" />
                            CSV
                          </Button>
                          <Button
                            variant="ghost" size="icon"
                            onClick={() => openEdit(selectedPool)}
                          >
                            <Pencil className="h-3 w-3" />
                          </Button>
                          <Button
                            variant="ghost" size="icon"
                            className="text-red-500"
                            onClick={() => handleDelete(selectedPool.id)}
                          >
                            <Trash2 className="h-3 w-3" />
                          </Button>
                        </div>
                      </div>
                      <CardDescription className="text-xs space-y-0.5">
                        <div className="flex items-center gap-2 flex-wrap">
                          <span>Rotation: <strong>{ROTATION_LABELS[selectedPool.rotation_method]}</strong>
                            {selectedPool.rotation_method === "stick" && ` (every ${selectedPool.stick_count} req)`}
                            {selectedPool.rotation_method === "session" && ` (idle ${selectedPool.session_ttl_minutes}m)`}
                          </span>
                          <Badge variant={selectedPool.sync_mode === "manual" ? "secondary" : "outline"} className="text-xs">
                            {selectedPool.sync_mode === "manual" ? "manual sync" : "auto sync"}
                          </Badge>
                        </div>
                        <div>Health check: <code className="text-xs bg-muted px-1 rounded">{selectedPool.health_check_cron}</code>
                          {" → "}<span className="text-muted-foreground">{selectedPool.health_check_url}</span>
                        </div>
                        {(selectedPool.geo_filters?.length ?? 0) > 0 && (
                          <div className="flex gap-1 flex-wrap items-center">
                            <Globe className="h-3 w-3 text-muted-foreground" />
                            {hasAllCountries(selectedPool.geo_filters)
                              ? <Badge variant="outline" className="text-xs">All countries</Badge>
                              : selectedPool.geo_filters!.map(f => (
                                <Badge key={geoFilterKey(f)} variant="outline" className="text-xs gap-1">
                                  <img src={FLAG_CDN(f.country_code)} alt={f.country_code} className="h-3" />
                                  {f.country_code}{f.city_name ? ` / ${f.city_name}` : ""}
                                </Badge>
                              ))}
                          </div>
                        )}
                        {(selectedPool.isp_filters?.length ?? 0) > 0 && (
                          <div className="flex gap-1 flex-wrap">
                            <span className="text-muted-foreground">ISP:</span>
                            {selectedPool.isp_filters!.map(i => <Badge key={i} variant="outline" className="text-xs">{i}</Badge>)}
                          </div>
                        )}
                        {(selectedPool.tag_filters?.length ?? 0) > 0 && (
                          <div className="flex gap-1 flex-wrap">
                            <Tag className="h-3 w-3 text-muted-foreground" />
                            {selectedPool.tag_filters!.map(t => <Badge key={t} variant="secondary" className="text-xs">{t}</Badge>)}
                          </div>
                        )}
                      </CardDescription>
                    </CardHeader>
                    {hcJob && (
                      <CardContent className="pt-0 pb-3">
                        <div className="rounded-md bg-muted p-3 text-xs space-y-2">
                          {/* Progress bar */}
                          {(hcJob.status === "running" || hcJob.status === "pending") && hcJob.total > 0 && (
                            <div>
                              <div className="flex justify-between mb-1">
                                <span className="text-muted-foreground">
                                  Checking {hcJob.progress}/{hcJob.total}…
                                </span>
                                <span className="text-muted-foreground">
                                  {Math.round((hcJob.progress / hcJob.total) * 100)}%
                                </span>
                              </div>
                              <Progress value={(hcJob.progress / hcJob.total) * 100} className="h-1.5" />
                            </div>
                          )}
                          <div className="flex gap-4 flex-wrap">
                            <span>Checked: <strong>{hcJob.progress}</strong>{hcJob.total > 0 && `/${hcJob.total}`}</span>
                            <span className="text-green-500">Active: <strong>{hcJob.active}</strong></span>
                            <span className="text-red-500">Failed: <strong>{hcJob.failed}</strong></span>
                            {hcJob.status === "running" && (
                              <span className="flex items-center gap-1 text-blue-500">
                                <Loader2 className="h-3 w-3 animate-spin" />running
                              </span>
                            )}
                            {hcJob.status === "done" && hcJob.finished_at && (
                              <span className="text-muted-foreground">
                                Done in {Math.round((new Date(hcJob.finished_at).getTime() - new Date(hcJob.started_at).getTime()) / 1000)}s
                              </span>
                            )}
                            {hcJob.status === "failed" && (
                              <span className="text-red-500">{hcJob.error}</span>
                            )}
                          </div>
                        </div>
                      </CardContent>
                    )}
                  </Card>

                  {/* Proxies in pool */}
                  <Card>
                    <CardHeader className="pb-2">
                      <div className="flex items-center justify-between">
                        <CardTitle className="text-sm">
                          Proxies in pool ({poolProxies.length})
                        </CardTitle>
                        <Button size="sm" variant="outline" onClick={openPicker}>
                          <Plus className="h-3 w-3 mr-1" />Add Proxies
                        </Button>
                      </div>
                    </CardHeader>
                    <CardContent className="p-0">
                      {poolProxiesLoading ? (
                        <div className="flex justify-center py-8">
                          <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
                        </div>
                      ) : poolProxies.length === 0 ? (
                        <p className="text-center py-6 text-sm text-muted-foreground">
                          No proxies. Use Sync to populate from the pool&apos;s filters, or add proxies manually.
                        </p>
                      ) : (
                        <div className="max-h-80 overflow-auto">
                          <Table>
                            <TableHeader>
                              <TableRow>
                                <TableHead className="text-xs">Address</TableHead>
                                <TableHead className="text-xs">Geo</TableHead>
                                <TableHead className="text-xs">Status</TableHead>
                                <TableHead className="text-xs text-right">RT</TableHead>
                                <TableHead className="w-8" />
                              </TableRow>
                            </TableHeader>
                            <TableBody>
                              {poolProxies.map(pp => (
                                <TableRow key={pp.proxy_id}>
                                  <TableCell className="text-xs font-mono">{pp.address}</TableCell>
                                  <TableCell className="text-xs">
                                    <span className="flex items-center gap-1">
                                      {pp.country_code && (
                                        <img src={FLAG_CDN(pp.country_code)} alt={pp.country_code} className="h-3" />
                                      )}
                                      {pp.city_name || pp.country_name || "—"}
                                    </span>
                                  </TableCell>
                                  <TableCell className="text-xs">
                                    <span className={statusColor(pp.status)}>
                                      {pp.status}
                                    </span>
                                  </TableCell>
                                  <TableCell className="text-xs text-right text-muted-foreground">
                                    {pp.avg_response_time ? `${pp.avg_response_time}ms` : "—"}
                                  </TableCell>
                                  <TableCell className="p-0 pr-2 text-right">
                                    <Button
                                      variant="ghost" size="icon"
                                      className="h-6 w-6 text-muted-foreground hover:text-red-500"
                                      title="Remove from pool"
                                      aria-label={`Remove ${pp.address} from pool`}
                                      onClick={() => handleRemoveProxyFromPool(pp.proxy_id)}
                                    >
                                      <X className="h-3 w-3" />
                                    </Button>
                                  </TableCell>
                                </TableRow>
                              ))}
                            </TableBody>
                          </Table>
                        </div>
                      )}
                    </CardContent>
                  </Card>

                  {/* Alert Rules */}
                  <Card>
                    <CardHeader className="pb-2">
                      <div className="flex items-center justify-between">
                        <CardTitle className="text-sm flex items-center gap-1">
                          <Bell className="h-3.5 w-3.5" />
                          Alert Rules ({alertRules.length})
                        </CardTitle>
                        <Button size="sm" variant="outline" onClick={openCreateAlertRule}>
                          <Plus className="h-3 w-3 mr-1" />Add Rule
                        </Button>
                      </div>
                    </CardHeader>
                    <CardContent className="p-0">
                      {alertRules.length === 0 ? (
                        <p className="text-center py-4 text-xs text-muted-foreground">
                          No alert rules. Add one to get notified when proxy count drops.
                        </p>
                      ) : (
                        <div className="divide-y">
                          {alertRules.map(rule => (
                            <div key={rule.id} className="flex items-center justify-between px-4 py-2 text-xs">
                              <div className="flex items-center gap-2">
                                {rule.enabled
                                  ? <Bell className="h-3 w-3 text-yellow-500" />
                                  : <BellOff className="h-3 w-3 text-muted-foreground" />
                                }
                                <span className="font-mono truncate max-w-[160px]" title={rule.webhook_url}>
                                  {rule.webhook_url}
                                </span>
                                <Badge variant="outline" className="text-xs">
                                  &lt; {rule.min_active_proxies} active
                                </Badge>
                              </div>
                              <div className="flex gap-1">
                                <Button variant="ghost" size="icon" className="h-6 w-6"
                                  onClick={() => openEditAlertRule(rule)}>
                                  <Pencil className="h-3 w-3" />
                                </Button>
                                <Button variant="ghost" size="icon" className="h-6 w-6 text-red-500"
                                  onClick={() => handleDeleteAlertRule(rule.id)}>
                                  <Trash2 className="h-3 w-3" />
                                </Button>
                              </div>
                            </div>
                          ))}
                        </div>
                      )}
                    </CardContent>
                  </Card>
                </div>
              ) : (
                <Card className="flex items-center justify-center min-h-[200px]">
                  <p className="text-muted-foreground text-sm">← Select a pool to view details</p>
                </Card>
              )}
            </div>
          )}
        </div>}

        {/* ── Geo distribution tab ─────────────────────────────────────────── */}
        {activeTab === "geo" && (
          <div className="mt-4">
            <GeoSelector
              countries={geoCountries}
              existingPools={pools}
              onCreated={loadAll}
            />
          </div>
        )}

      {/* ── Create / Edit pool dialog ──────────────────────────────────────── */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{editPool ? "Edit Pool" : "Create Proxy Pool"}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-4 py-2">
            <div className="grid grid-cols-2 gap-3">
              <div className="col-span-2 flex flex-col gap-1.5">
                <Label>Name</Label>
                <Input
                  placeholder="e.g. US East"
                  value={form.name}
                  onChange={e => setForm({ ...form, name: e.target.value })}
                />
              </div>
              <div className="col-span-2 flex flex-col gap-1.5">
                <Label>Description</Label>
                <Input
                  placeholder="Optional description"
                  value={form.description}
                  onChange={e => setForm({ ...form, description: e.target.value })}
                />
              </div>

              {/* Multi-country geo filters — a pool matches proxies from any of the listed
                  countries, or takes every proxy (GEO_FILTER_ALL) and follows them wherever
                  their IPs move */}
              <div className="col-span-2 flex flex-col gap-2 border rounded-md p-3">
                <div className="flex items-center justify-between">
                  <Label className="text-sm font-medium">Countries in this pool</Label>
                  <span className="text-xs text-muted-foreground">
                    {formAllCountries ? "all countries" : `${formGeoCount} filter${formGeoCount === 1 ? "" : "s"}`}
                  </span>
                </div>

                {formAllCountries ? (
                  <>
                    <div className="flex flex-wrap gap-1.5 min-h-[28px]">
                      <Badge variant="secondary" className="gap-1 pr-1">
                        <Globe className="h-3 w-3" />
                        <span className="text-xs">All countries</span>
                        <button
                          type="button"
                          className="ml-0.5 rounded hover:bg-muted-foreground/20 px-1 text-xs leading-none"
                          onClick={() => setForm({ ...form, geo_filters: [] })}
                          title="Pick specific countries instead"
                        >
                          ×
                        </button>
                      </Badge>
                    </div>
                    <p className="text-xs text-muted-foreground">
                      Every proxy in inventory joins this pool and stays in it wherever its IP moves,
                      including proxies whose location hasn't been resolved yet. Remove this to pick
                      specific countries.
                    </p>
                  </>
                ) : (
                  <>
                    {/* Existing filters as removable badges */}
                    <div className="flex flex-wrap gap-1.5 min-h-[28px]">
                      {formGeoCount === 0 && (
                        <span className="text-xs text-muted-foreground italic">
                          No country filters. Add one below, pick All countries, or select proxies by ISP or tag.
                        </span>
                      )}
                      {(form.geo_filters ?? []).map((f, idx) => (
                        <Badge key={`${f.country_code}-${f.city_name ?? ""}-${idx}`} variant="secondary" className="gap-1 pr-1">
                          <img src={FLAG_CDN(f.country_code)} alt={f.country_code} className="h-3" />
                          <span className="font-mono text-xs">{f.country_code.toUpperCase()}</span>
                          {f.city_name && <span className="text-xs text-muted-foreground">/ {f.city_name}</span>}
                          <button
                            type="button"
                            className="ml-0.5 rounded hover:bg-muted-foreground/20 px-1 text-xs leading-none"
                            onClick={() => setForm({
                              ...form,
                              geo_filters: (form.geo_filters ?? []).filter((_, i) => i !== idx),
                            })}
                            title="Remove"
                          >
                            ×
                          </button>
                        </Badge>
                      ))}
                    </div>

                    {/* Quick-add row: country code + optional city + Add button */}
                    <div className="flex gap-2 items-end pt-1">
                      <div className="flex flex-col gap-1 flex-1">
                        <Label htmlFor="new-geo-cc" className="text-xs">Country</Label>
                        <Input
                          id="new-geo-cc"
                          placeholder="BR"
                          maxLength={3}
                          value={newGeoCountry}
                          onChange={e => setNewGeoCountry(e.target.value.toUpperCase())}
                          onKeyDown={e => {
                            if (e.key === "Enter") {
                              e.preventDefault()
                              const cc = newGeoCountry.trim().toUpperCase()
                              if (!cc) return
                              const existing = form.geo_filters ?? []
                              if (existing.some(f => f.country_code === cc && (f.city_name ?? "") === newGeoCity.trim())) {
                                toast.error("Already added")
                                return
                              }
                              setForm({
                                ...form,
                                geo_filters: [...existing, { country_code: cc, ...(newGeoCity.trim() ? { city_name: newGeoCity.trim() } : {}) }],
                              })
                              setNewGeoCountry("")
                              setNewGeoCity("")
                            }
                          }}
                        />
                      </div>
                      <div className="flex flex-col gap-1 flex-[2]">
                        <Label htmlFor="new-geo-city" className="text-xs">City (optional)</Label>
                        <Input
                          id="new-geo-city"
                          placeholder="Leave empty for whole country"
                          value={newGeoCity}
                          onChange={e => setNewGeoCity(e.target.value)}
                        />
                      </div>
                      <Button
                        type="button"
                        size="sm"
                        onClick={() => {
                          const cc = newGeoCountry.trim().toUpperCase()
                          if (!cc) { toast.error("Enter a country code"); return }
                          const existing = form.geo_filters ?? []
                          if (existing.some(f => f.country_code === cc && (f.city_name ?? "") === newGeoCity.trim())) {
                            toast.error("Already added")
                            return
                          }
                          setForm({
                            ...form,
                            geo_filters: [...existing, { country_code: cc, ...(newGeoCity.trim() ? { city_name: newGeoCity.trim() } : {}) }],
                          })
                          setNewGeoCountry("")
                          setNewGeoCity("")
                        }}
                      >
                        <Plus className="h-4 w-4 mr-1" /> Add
                      </Button>
                    </div>

                    {/* Quick-pick: everything, or common countries from existing proxy inventory */}
                    <div className="pt-2 border-t mt-1 flex flex-col gap-1.5">
                      <Label className="text-xs text-muted-foreground">Quick pick from inventory</Label>
                      <div className="flex flex-wrap gap-1">
                        <Button
                          type="button"
                          size="sm"
                          variant="outline"
                          className="h-6 px-2 text-xs"
                          onClick={() => {
                            if (formGeoCount > 0) toast(`Replaced ${formGeoCount} country filter${formGeoCount === 1 ? "" : "s"} with All countries`)
                            setForm({ ...form, geo_filters: [{ country_code: GEO_FILTER_ALL }] })
                          }}
                          title="Match every proxy regardless of country — the pool follows proxies when their IPs move"
                        >
                          <Globe className="h-3 w-3 mr-1" />
                          All countries
                        </Button>
                        {geoCountries.slice(0, 12).map(gc => {
                          const already = (form.geo_filters ?? []).some(f => f.country_code === gc.country_code && !f.city_name)
                          return (
                            <Button
                              key={gc.country_code}
                              type="button"
                              size="sm"
                              variant={already ? "secondary" : "outline"}
                              disabled={already}
                              className="h-6 px-2 text-xs"
                              onClick={() => setForm({
                                ...form,
                                geo_filters: [...(form.geo_filters ?? []), { country_code: gc.country_code }],
                              })}
                              title={`${gc.country_name ?? gc.country_code} — ${gc.total} proxies`}
                            >
                              <img src={FLAG_CDN(gc.country_code)} alt={gc.country_code} className="h-3 mr-1" />
                              {gc.country_code}
                              <span className="ml-1 text-muted-foreground">({gc.total})</span>
                            </Button>
                          )
                        })}
                      </div>
                    </div>
                  </>
                )}
              </div>

              {/* Tag filters — proxies carrying every listed tag join, including
                  ones without GeoIP data (local or VPN proxies) */}
              <div className="col-span-2 flex flex-col gap-2 border rounded-md p-3">
                <div className="flex items-center justify-between">
                  <Label className="text-sm font-medium flex items-center gap-1.5">
                    <Tag className="h-3.5 w-3.5" />Tag filters
                  </Label>
                  <span className="text-xs text-muted-foreground">
                    {form.tag_filters?.length ?? 0} filter{(form.tag_filters?.length ?? 0) === 1 ? "" : "s"}
                  </span>
                </div>
                <TagInput
                  value={form.tag_filters ?? []}
                  onChange={tag_filters => setForm({ ...form, tag_filters })}
                  suggestions={tagListQuery.data ?? []}
                  placeholder="e.g. residential"
                  emptyText="No tag filters"
                />
                <p className="text-xs text-muted-foreground">
                  Proxies carrying <strong>all</strong> of these tags join the pool. Tag proxies on the Proxy Management page.
                </p>
              </div>

              {/* ISP filters — substring match against the proxy's GeoIP ISP */}
              <div className="col-span-2 flex flex-col gap-2 border rounded-md p-3">
                <div className="flex items-center justify-between">
                  <Label className="text-sm font-medium">ISP filters</Label>
                  <span className="text-xs text-muted-foreground">
                    {form.isp_filters?.length ?? 0} filter{(form.isp_filters?.length ?? 0) === 1 ? "" : "s"}
                  </span>
                </div>
                <TagInput
                  value={form.isp_filters ?? []}
                  onChange={isp_filters => setForm({ ...form, isp_filters })}
                  suggestions={ispListQuery.data ?? []}
                  placeholder="e.g. Comcast"
                  emptyText="No ISP filters"
                />
                <p className="text-xs text-muted-foreground">
                  Proxies whose ISP contains <strong>any</strong> of these names join the pool.
                  A proxy matching any country, ISP or tag filter becomes a member.
                </p>
              </div>

              {/* Full-width: the session label is too long for half the dialog */}
              <div className="col-span-2 flex flex-col gap-1.5 min-w-0">
                <Label>Rotation strategy</Label>
                <Select
                  value={form.rotation_method}
                  onValueChange={v => setForm({ ...form, rotation_method: v as CreatePoolRequest["rotation_method"] })}
                >
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="roundrobin">Round Robin</SelectItem>
                    <SelectItem value="random">Random</SelectItem>
                    <SelectItem value="stick">Sticky (N requests)</SelectItem>
                    <SelectItem value="session">Session (sticky until released/idle)</SelectItem>
                  </SelectContent>
                </Select>
              </div>

              {form.rotation_method === "stick" && (
                <div className="col-span-2 flex flex-col gap-1.5">
                  <Label>Stick requests count</Label>
                  <Input
                    type="number"
                    min={1}
                    value={form.stick_count}
                    onChange={e => setForm({ ...form, stick_count: parseInt(e.target.value) || 10 })}
                  />
                </div>
              )}

              {form.rotation_method === "session" && (
                <div className="col-span-2 flex flex-col gap-1.5">
                  <Label>Session idle TTL (minutes)</Label>
                  <Input
                    type="number"
                    min={1}
                    value={form.session_ttl_minutes}
                    onChange={e => setForm({ ...form, session_ttl_minutes: parseInt(e.target.value) || 10 })}
                  />
                  <p className="text-xs text-muted-foreground">
                    A session keeps its proxy until released or idle this long.
                    Clients pick a session via the proxy username:
                    <code className="ml-1">user-session-&lt;id&gt;</code>
                    {" "}Each session reserves an exclusive proxy per target hostname.
                    Add <code>-scope-&lt;group&gt;</code> to share a reservation scope
                    across hostnames. When no proxy is available, clients receive
                    593 (No Proxy Available) with a Retry-After header.
                  </p>
                </div>
              )}

              <div className="col-span-2 flex flex-col gap-1.5">
                <Label>Health check URL</Label>
                <Input
                  placeholder="https://api.ipify.org"
                  value={form.health_check_url}
                  onChange={e => setForm({ ...form, health_check_url: e.target.value })}
                />
              </div>
              <div className="col-span-2 flex flex-col gap-1.5">
                <Label>Health check cron</Label>
                <Input
                  placeholder="*/30 * * * *"
                  value={form.health_check_cron}
                  onChange={e => setForm({ ...form, health_check_cron: e.target.value })}
                />
                <p className="text-xs text-muted-foreground">
                  e.g. <code>*/30 * * * *</code> = every 30 min
                </p>
              </div>
            </div>

            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-2">
                <Switch
                  id="hc-enabled"
                  checked={form.health_check_enabled}
                  onCheckedChange={v => setForm({ ...form, health_check_enabled: v })}
                />
                <Label htmlFor="hc-enabled">Auto health check</Label>
              </div>
              <div className="flex items-center gap-2">
                <Switch
                  id="auto-sync"
                  checked={form.auto_sync}
                  onCheckedChange={v => setForm({ ...form, auto_sync: v })}
                />
                <Label htmlFor="auto-sync">Auto-sync membership on import</Label>
              </div>
              <div className="flex items-center gap-2">
                <Switch
                  id="sync-manual"
                  checked={form.sync_mode === "manual"}
                  onCheckedChange={v => setForm({ ...form, sync_mode: v ? "manual" : "auto" })}
                />
                <Label htmlFor="sync-manual">Manual sync mode (don&apos;t auto-rebuild on import)</Label>
              </div>
              <div className="flex items-center gap-2">
                <Switch
                  id="pool-enabled"
                  checked={form.enabled}
                  onCheckedChange={v => setForm({ ...form, enabled: v })}
                />
                <Label htmlFor="pool-enabled">Enabled</Label>
              </div>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>Cancel</Button>
            <Button onClick={handleSave} disabled={saving}>
              {saving && <Loader2 className="h-4 w-4 animate-spin mr-2" />}
              {editPool ? "Save Changes" : "Create Pool"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Add proxies to pool dialog */}
      <Dialog open={pickerOpen} onOpenChange={setPickerOpen}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle>Add proxies to {selectedPool?.name}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-3 py-2">
            {selectedPool && rebuildsFromFilters(selectedPool) && (
              <p className="text-xs rounded-md border border-yellow-500/40 bg-yellow-500/10 p-2 text-yellow-600 dark:text-yellow-400">
                This pool rebuilds its members from its filters on every sync, which drops
                added proxies that don&apos;t match them. Switch the pool to manual sync mode,
                or tag the proxies and add a matching tag filter, to keep them.
              </p>
            )}
            <Input
              placeholder="Search by address…"
              value={pickerSearch}
              onChange={e => setPickerSearch(e.target.value)}
            />
            <div className="border rounded-md max-h-64 overflow-auto">
              {pickerResults.isLoading ? (
                <div className="flex justify-center py-8">
                  <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
                </div>
              ) : (pickerResults.data ?? []).length === 0 ? (
                <p className="text-center py-6 text-sm text-muted-foreground">No proxies found</p>
              ) : (
                <div className="divide-y">
                  {(pickerResults.data ?? []).map(p => {
                    const inPool = poolProxies.some(pp => pp.proxy_id === p.id)
                    const checked = pickerSelected.includes(p.id)
                    return (
                      <label
                        key={p.id}
                        className={`flex items-center gap-2 px-3 py-1.5 text-xs ${inPool ? "opacity-50" : "cursor-pointer hover:bg-muted/50"}`}
                      >
                        <Checkbox
                          checked={inPool || checked}
                          disabled={inPool}
                          onCheckedChange={v => setPickerSelected(prev =>
                            v ? [...prev, p.id] : prev.filter(id => id !== p.id)
                          )}
                        />
                        <span className="font-mono flex-1 truncate">{p.address}</span>
                        <Badge variant="outline" className="text-xs uppercase">{p.protocol}</Badge>
                        {(p.tags ?? []).slice(0, 2).map(t => (
                          <Badge key={t} variant="secondary" className="text-xs">{t}</Badge>
                        ))}
                        <span className={statusColor(p.status)}>{inPool ? "in pool" : p.status}</span>
                      </label>
                    )
                  })}
                </div>
              )}
            </div>
            <p className="text-xs text-muted-foreground">Showing up to 50 matches; search to narrow the list.</p>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPickerOpen(false)} disabled={addingProxies}>
              Cancel
            </Button>
            <Button
              onClick={handleAddProxiesToPool}
              disabled={addingProxies || pickerSelected.length === 0}
            >
              {addingProxies && <Loader2 className="h-4 w-4 animate-spin mr-2" />}
              Add {pickerSelected.length || ""} {pickerSelected.length === 1 ? "Proxy" : "Proxies"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Alert Rule Dialog */}
      <Dialog open={alertDialogOpen} onOpenChange={setAlertDialogOpen}>
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>{editAlertRule ? "Edit Alert Rule" : "New Alert Rule"}</DialogTitle>
          </DialogHeader>
          <div className="space-y-3 py-2">
            <div>
              <Label className="text-xs">Webhook URL</Label>
              <Input
                placeholder="https://hooks.slack.com/..."
                value={alertForm.webhook_url}
                onChange={e => setAlertForm({ ...alertForm, webhook_url: e.target.value })}
              />
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div>
                <Label className="text-xs">Min Active Proxies</Label>
                <Input
                  type="number" min={1}
                  value={alertForm.min_active_proxies}
                  onChange={e => setAlertForm({ ...alertForm, min_active_proxies: parseInt(e.target.value) || 1 })}
                />
                <p className="text-xs text-muted-foreground mt-1">Fire when active drops below this</p>
              </div>
              <div>
                <Label className="text-xs">Cooldown (minutes)</Label>
                <Input
                  type="number" min={1}
                  value={alertForm.cooldown_minutes}
                  onChange={e => setAlertForm({ ...alertForm, cooldown_minutes: parseInt(e.target.value) || 30 })}
                />
              </div>
            </div>
            <div className="flex items-center gap-2">
              <Switch
                checked={alertForm.enabled}
                onCheckedChange={v => setAlertForm({ ...alertForm, enabled: v })}
              />
              <Label className="text-xs">Enabled</Label>
            </div>
            <div className="rounded-md bg-muted p-3 text-xs text-muted-foreground">
              <strong>Payload sent:</strong><br />
              {"{ event: \"pool.degraded\", pool_id, pool_name, active_proxies, total_proxies, threshold, fired_at }"}
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAlertDialogOpen(false)}>Cancel</Button>
            <Button onClick={handleSaveAlertRule} disabled={savingAlert}>
              {savingAlert && <Loader2 className="h-4 w-4 animate-spin mr-2" />}
              {editAlertRule ? "Save" : "Create Rule"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

    </div>
  )
}
