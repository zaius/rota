"use client"

import * as React from "react"
import {
  ColumnDef,
  ColumnFiltersState,
  SortingState,
  VisibilityState,
  flexRender,
  getCoreRowModel,
  getFilteredRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
} from "@tanstack/react-table"
import {
  ArrowUpDown,
  ChevronDown,
  MoreHorizontal,
  Plus,
  Download,
  Trash2,
  Loader2,
  Upload,
  FileText,
  CheckCircle2,
  XCircle,
  AlertCircle,
  Filter,
  Activity,
} from "lucide-react"

import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Status, StatusIndicator, StatusLabel } from "@/components/ui/shadcn-io/status"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
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
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { api } from "@/lib/api"
import { Proxy, AddProxyRequest, ProxyFilter } from "@/lib/types"
import { toast } from "@/lib/toast"

const PROXY_PROTOCOLS = ["http", "https", "socks4", "socks4a", "socks5"] as const

// parseProxyLine parses a single line from a bulk-import file into a proxy
// request. Credentials and protocol are expected to be embedded in the URL.
// Supported formats (auth and protocol are optional):
//
//   host:port
//   user:pass@host:port
//   protocol://host:port
//   protocol://user:pass@host:port
//
// Returns null for blank lines, comments (#...), and lines without a port.
function parseProxyLine(raw: string): AddProxyRequest | null {
  let line = raw.trim()
  if (line === "" || line.startsWith("#")) return null

  let protocol: AddProxyRequest["protocol"] = "http"
  let username: string | undefined
  let password: string | undefined

  // Strip protocol scheme if present.
  const schemeIdx = line.indexOf("://")
  if (schemeIdx !== -1) {
    const scheme = line.slice(0, schemeIdx).toLowerCase()
    if ((PROXY_PROTOCOLS as readonly string[]).includes(scheme)) {
      protocol = scheme as AddProxyRequest["protocol"]
    }
    line = line.slice(schemeIdx + 3)
  }

  // Split optional user:pass@ userinfo from the host.
  let hostPort = line
  const atIdx = line.lastIndexOf("@")
  if (atIdx !== -1) {
    const userinfo = line.slice(0, atIdx)
    hostPort = line.slice(atIdx + 1)
    const colonIdx = userinfo.indexOf(":")
    if (colonIdx !== -1) {
      username = userinfo.slice(0, colonIdx) || undefined
      password = userinfo.slice(colonIdx + 1) || undefined
    } else {
      username = userinfo || undefined
    }
  }

  // Require host:port with a numeric port.
  const portParts = hostPort.split(":")
  if (portParts.length < 2 || !/^\d+$/.test(portParts[portParts.length - 1])) {
    return null
  }

  return { address: hostPort, protocol, username, password }
}

export default function ProxiesPage() {
  const [data, setData] = React.useState<Proxy[]>([])
  const [isLoading, setIsLoading] = React.useState(true)
  const [isAddDialogOpen, setIsAddDialogOpen] = React.useState(false)
  const [isEditDialogOpen, setIsEditDialogOpen] = React.useState(false)
  const [isImportDialogOpen, setIsImportDialogOpen] = React.useState(false)
  const [editingProxy, setEditingProxy] = React.useState<Proxy | null>(null)
  const [sorting, setSorting] = React.useState<SortingState>([])
  const [columnFilters, setColumnFilters] = React.useState<ColumnFiltersState>([])
  const [columnVisibility, setColumnVisibility] = React.useState<VisibilityState>({})
  const [rowSelection, setRowSelection] = React.useState({})
  const [pagination, setPagination] = React.useState({
    page: 1,
    limit: 10,
    total: 0,
    total_pages: 0,
  })
  const [searchQuery, setSearchQuery] = React.useState("")
  const [debouncedSearchQuery, setDebouncedSearchQuery] = React.useState("")
  const [statusFilter, setStatusFilter] = React.useState<string>("all")
  const [protocolFilter, setProtocolFilter] = React.useState<string>("all")

  const [newProxy, setNewProxy] = React.useState({
    address: "",
    protocol: "http" as "http" | "https" | "socks5",
    username: "",
    password: "",
  })

  // Import modal states
  const [importFile, setImportFile] = React.useState<File | null>(null)
  const [parsedProxies, setParsedProxies] = React.useState<AddProxyRequest[]>([])
  const [isImporting, setIsImporting] = React.useState(false)
  const [importProgress, setImportProgress] = React.useState({ current: 0, total: 0, success: 0, failed: 0, skipped: 0 })
  const [importResults, setImportResults] = React.useState<Array<{ address: string; status: string; error?: string }>>([])
  const [isDragging, setIsDragging] = React.useState(false)
  const [isReloading, setIsReloading] = React.useState(false)
  const [deleteConfirm, setDeleteConfirm] = React.useState<{ open: boolean; proxyId: number | null }>({ open: false, proxyId: null })
   const [bulkDeleteConfirm, setBulkDeleteConfirm] = React.useState(false)
   const [deleteAllConfirm, setDeleteAllConfirm] = React.useState(false)
  const [isBulkTesting, setIsBulkTesting] = React.useState(false)
  const [selectAllMatching, setSelectAllMatching] = React.useState(false)

  // Debounce search query
  React.useEffect(() => {
    const timer = setTimeout(() => {
      setDebouncedSearchQuery(searchQuery)
      setPagination(prev => ({ ...prev, page: 1 }))
    }, 500)

    return () => clearTimeout(timer)
  }, [searchQuery])

  const fetchProxies = React.useCallback(async () => {
    try {
      setIsLoading(true)

      // Build sort parameters from sorting state
      const sortField = sorting.length > 0 ? sorting[0].id : undefined
      const sortOrder = sorting.length > 0 ? (sorting[0].desc ? "desc" : "asc") : undefined

      const response = await api.getProxies({
        page: pagination.page,
        limit: pagination.limit,
        search: debouncedSearchQuery || undefined,
        status: statusFilter === "all" ? undefined : statusFilter,
        protocol: protocolFilter === "all" ? undefined : protocolFilter,
        sort: sortField,
        order: sortOrder as "asc" | "desc" | undefined,
      })
      setData(response.proxies)
      setPagination(response.pagination)
    } catch (error) {
      console.error("Failed to fetch proxies:", error)
    } finally {
      setIsLoading(false)
    }
  }, [pagination.page, pagination.limit, debouncedSearchQuery, statusFilter, protocolFilter, sorting])

  React.useEffect(() => {
    fetchProxies()
  }, [fetchProxies])

  const handleAddProxy = async () => {
    try {
      await api.addProxy(newProxy)
      setIsAddDialogOpen(false)
      setNewProxy({ address: "", protocol: "http", username: "", password: "" })
      toast.success("Proxy added successfully")
      fetchProxies()
    } catch (error) {
      console.error("Failed to add proxy:", error)
      toast.error("Failed to add proxy", error instanceof Error ? error.message : "Unknown error")
    }
  }

  const handleEditProxy = async () => {
    if (!editingProxy) return

    try {
      await api.updateProxy(editingProxy.id, {
        address: editingProxy.address,
        protocol: editingProxy.protocol,
        username: editingProxy.username,
      })
      setIsEditDialogOpen(false)
      setEditingProxy(null)
      toast.success("Proxy updated successfully")
      fetchProxies()
    } catch (error) {
      console.error("Failed to update proxy:", error)
      toast.error("Failed to update proxy", error instanceof Error ? error.message : "Unknown error")
    }
  }

  const handleDeleteProxy = async (id: number) => {
    setDeleteConfirm({ open: true, proxyId: id })
  }

  const confirmDelete = async () => {
    if (!deleteConfirm.proxyId) return

    try {
      await api.deleteProxy(deleteConfirm.proxyId)
      toast.success("Proxy deleted successfully")
      fetchProxies()
    } catch (error) {
      console.error("Failed to delete proxy:", error)
      toast.error("Failed to delete proxy", error instanceof Error ? error.message : "Unknown error")
    } finally {
      setDeleteConfirm({ open: false, proxyId: null })
    }
  }

  const handleTestProxy = async (id: number) => {
    try {
      const result = await api.testProxy(id)
      if (result.status === "active") {
        const responseTime = result.response_time || result.duration || 0
        toast.success(
          "Proxy test successful",
          `${result.address} - Response time: ${responseTime}ms`
        )
      } else {
        toast.error(
          "Proxy test failed",
          `${result.address} - ${result.error || "Unknown error"}`
        )
      }
      fetchProxies()
    } catch (error) {
      console.error("Failed to test proxy:", error)
      toast.error("Failed to test proxy", error instanceof Error ? error.message : "Unknown error")
    }
  }

  const handleInvalidateProxy = async (id: number) => {
    try {
      const res = await api.invalidateProxy(id, { reason: "manually invalidated" })
      toast.success("Proxy invalidated", `${res.address} pulled out of rotation`)
      fetchProxies()
    } catch (error) {
      console.error("Failed to invalidate proxy:", error)
      toast.error("Failed to invalidate proxy", error instanceof Error ? error.message : "Unknown error")
    }
  }

  const handleReactivateProxy = async (id: number) => {
    try {
      await api.reactivateProxy(id)
      toast.success("Proxy reactivated", "Returned to rotation")
      fetchProxies()
    } catch (error) {
      console.error("Failed to reactivate proxy:", error)
      toast.error("Failed to reactivate proxy", error instanceof Error ? error.message : "Unknown error")
    }
  }

  const handleBulkDelete = async () => {
    if (selectedCount === 0) return
    setBulkDeleteConfirm(true)
  }

  const confirmBulkDelete = async () => {
    try {
      const res = selectAllMatching
        ? await api.bulkDeleteProxies({ all: true, filter: currentFilter() })
        : await api.bulkDeleteProxies({
            ids: Object.keys(rowSelection).map(key => data[Number(key)].id),
          })
      setRowSelection({})
      setSelectAllMatching(false)
      toast.success(`${res.deleted} proxies deleted successfully`)
      fetchProxies()
    } catch (error) {
      console.error("Failed to delete proxies:", error)
      toast.error("Failed to delete proxies", error instanceof Error ? error.message : "Unknown error")
    } finally {
      setBulkDeleteConfirm(false)
    }
  }

  const handleBulkTest = async () => {
    const selectedIds = Object.keys(rowSelection).map(key => data[Number(key)].id)
    if (!selectAllMatching && selectedIds.length === 0) return

    setIsBulkTesting(true)
    try {
      const result = selectAllMatching
        ? await api.bulkTestProxies({ all: true, filter: currentFilter() })
        : await api.bulkTestProxies({ ids: selectedIds })

      const detail = [`${result.active} active`, `${result.failed} failed`]
      if (result.skipped > 0) {
        detail.push(`${result.skipped} skipped (1000 max per run)`)
      }
      if (result.failed === 0 && result.skipped === 0) {
        toast.success(`Tested ${result.tested} proxies`, detail.join(", "))
      } else {
        toast.error(`Tested ${result.tested} proxies`, detail.join(", "))
      }
      fetchProxies()
    } catch (error) {
      console.error("Failed to test proxies:", error)
      toast.error("Failed to test proxies", error instanceof Error ? error.message : "Unknown error")
    } finally {
      setIsBulkTesting(false)
    }
  }

  const confirmDeleteAll = async () => {
    try {
      const res = await api.deleteAllProxies()
      setRowSelection({})
      toast.success(`${res.deleted} proxies deleted`)
      fetchProxies()
    } catch (error) {
      toast.error("Failed to delete all proxies")
    } finally {
      setDeleteAllConfirm(false)
    }
  }

  const handleExport = async (format: "txt" | "json" | "csv") => {
    try {
      const blob = await api.exportProxies(format, currentFilter())
      const url = URL.createObjectURL(blob)
      const a = document.createElement("a")
      a.href = url
      a.download = `proxies.${format}`
      a.click()
      URL.revokeObjectURL(url)
      toast.success(`Proxies exported as ${format.toUpperCase()}`)
    } catch (error) {
      console.error("Failed to export proxies:", error)
      toast.error("Failed to export proxies", error instanceof Error ? error.message : "Unknown error")
    }
  }

  const handleFileUpload = (file: File) => {
    if (!file.name.endsWith('.txt')) {
      toast.error('Invalid file type', 'Please upload a .txt file')
      return
    }

    const reader = new FileReader()
    reader.onload = (e) => {
      const text = e.target?.result as string
      const proxies = text.split('\n')
        .map(parseProxyLine)
        .filter((proxy): proxy is AddProxyRequest => proxy !== null)

      setParsedProxies(proxies)
      setImportFile(file)
    }
    reader.readAsText(file)
  }

  const handleDragOver = (e: React.DragEvent) => {
    e.preventDefault()
    setIsDragging(true)
  }

  const handleDragLeave = () => {
    setIsDragging(false)
  }

  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault()
    setIsDragging(false)

    const files = Array.from(e.dataTransfer.files)
    const txtFile = files.find(f => f.name.endsWith('.txt'))

    if (txtFile) {
      handleFileUpload(txtFile)
    } else {
      toast.error('Invalid file type', 'Please upload a .txt file')
    }
  }

  const handleImport = async () => {
    if (parsedProxies.length === 0) {
      toast.error('No proxies to import', 'No valid proxies found in the file')
      return
    }

    setIsImporting(true)
    setImportProgress({ current: 0, total: parsedProxies.length, success: 0, failed: 0, skipped: 0 })
    setImportResults([])

    const results: Array<{ address: string; status: string; error?: string }> = []
    let success = 0
    let failed = 0
    let skipped = 0

    for (let i = 0; i < parsedProxies.length; i++) {
      const proxy = parsedProxies[i]
      const { address } = proxy

      try {
        await api.addProxy(proxy)

        success++
        results.push({ address, status: 'success' })
      } catch (error) {
        const errorMessage = error instanceof Error ? error.message : 'Unknown error'

        // Check if it's a duplicate error
        if (errorMessage.includes('already exists')) {
          skipped++
          results.push({
            address,
            status: 'skipped',
            error: 'Already exists (skipped)'
          })
        } else {
          failed++
          results.push({
            address,
            status: 'failed',
            error: errorMessage
          })
        }
      }

      setImportProgress({
        current: i + 1,
        total: parsedProxies.length,
        success,
        failed,
        skipped,
      })
      setImportResults([...results])
    }

    setIsImporting(false)
    // Refresh the proxy list
    setTimeout(() => {
      fetchProxies()
    }, 1000)
  }

  const resetImportDialog = () => {
    setImportFile(null)
    setParsedProxies([])
    setIsImporting(false)
    setImportProgress({ current: 0, total: 0, success: 0, failed: 0, skipped: 0 })
    setImportResults([])
  }

  const handleReloadProxies = async () => {
    try {
      setIsReloading(true)
      await api.reloadProxies()
      toast.success('Proxy pool reloaded', 'All proxies from database are now available for rotation')
    } catch (error) {
      console.error('Failed to reload proxies:', error)
      toast.error('Failed to reload proxy pool', error instanceof Error ? error.message : "Unknown error")
    } finally {
      setIsReloading(false)
    }
  }

  const columns: ColumnDef<Proxy>[] = [
    {
      id: "select",
      header: ({ table }) => (
        <Checkbox
          checked={
            table.getIsAllPageRowsSelected() ||
            (table.getIsSomePageRowsSelected() && "indeterminate")
          }
          onCheckedChange={(value) => table.toggleAllPageRowsSelected(!!value)}
          aria-label="Select all"
        />
      ),
      cell: ({ row }) => (
        <Checkbox
          checked={row.getIsSelected()}
          onCheckedChange={(value) => row.toggleSelected(!!value)}
          aria-label="Select row"
        />
      ),
      enableSorting: false,
      enableHiding: false,
    },
    {
      accessorKey: "address",
      header: ({ column }) => {
        return (
          <Button
            variant="ghost"
            onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}
          >
            Address
            <ArrowUpDown className="ml-2 h-4 w-4" />
          </Button>
        )
      },
      cell: ({ row }) => <div className="font-mono">{row.getValue("address")}</div>,
    },
    {
      accessorKey: "protocol",
      header: "Protocol",
      cell: ({ row }) => (
        <Badge variant="outline" className="uppercase">
          {row.getValue("protocol")}
        </Badge>
      ),
    },
    {
      accessorKey: "status",
      header: ({ column }) => {
        return (
          <Button
            variant="ghost"
            onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}
          >
            Status
            <ArrowUpDown className="ml-2 h-4 w-4" />
          </Button>
        )
      },
      cell: ({ row }) => {
        const status = row.getValue("status") as string
        const statusMap = {
          active: "online" as const,
          failed: "offline" as const,
          idle: "idle" as const,
        }
        const statusColors = {
          active: "text-green-600",
          failed: "text-red-600",
          idle: "text-yellow-600",
        }
        return (
          <div className={`flex items-center gap-2 ${statusColors[status as keyof typeof statusColors]}`}>
            <div className={`h-2 w-2 rounded-full ${
              status === 'active' ? 'bg-green-600' :
              status === 'failed' ? 'bg-red-600' :
              'bg-yellow-600'
            }`} />
            <span className="capitalize font-medium">{status}</span>
          </div>
        )
      },
    },
    {
      accessorKey: "requests",
      header: ({ column }) => {
        return (
          <Button
            variant="ghost"
            onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}
          >
            Requests
            <ArrowUpDown className="ml-2 h-4 w-4" />
          </Button>
        )
      },
      cell: ({ row }) => {
        const value = parseFloat(row.getValue("requests"))
        return <div suppressHydrationWarning>{value.toLocaleString('en-US')}</div>
      },
    },
    {
      accessorKey: "success_rate",
      header: ({ column }) => {
        return (
          <Button
            variant="ghost"
            onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}
          >
            Success Rate
            <ArrowUpDown className="ml-2 h-4 w-4" />
          </Button>
        )
      },
      cell: ({ row }) => {
        const value = parseFloat(row.getValue("success_rate"))
        return <div>{value.toFixed(1)}%</div>
      },
    },
    {
      accessorKey: "avg_response_time",
      header: "Avg Response",
      cell: ({ row }) => {
        const value = parseFloat(row.getValue("avg_response_time"))
        return <div>{value}ms</div>
      },
    },
    {
      accessorKey: "last_check",
      header: "Last Check",
    },
    {
      id: "actions",
      enableHiding: false,
      cell: ({ row }) => {
        const proxy = row.original

        return (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" className="h-8 w-8 p-0">
                <span className="sr-only">Open menu</span>
                <MoreHorizontal className="h-4 w-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuLabel>Actions</DropdownMenuLabel>
              <DropdownMenuItem
                onClick={() => navigator.clipboard.writeText(proxy.address)}
              >
                Copy address
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem onClick={() => handleTestProxy(proxy.id)}>
                Test proxy
              </DropdownMenuItem>
              {proxy.cooldown_until && new Date(proxy.cooldown_until) > new Date() ? (
                <DropdownMenuItem onClick={() => handleReactivateProxy(proxy.id)}>
                  Reactivate (clear cooldown)
                </DropdownMenuItem>
              ) : (
                <DropdownMenuItem onClick={() => handleInvalidateProxy(proxy.id)}>
                  Invalidate (rate-limited)
                </DropdownMenuItem>
              )}
              <DropdownMenuItem onClick={() => {
                setEditingProxy(proxy)
                setIsEditDialogOpen(true)
              }}>
                Edit
              </DropdownMenuItem>
              <DropdownMenuItem
                className="text-red-600"
                onClick={() => handleDeleteProxy(proxy.id)}
              >
                Delete
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        )
      },
    },
  ]

  const table = useReactTable({
    data,
    columns,
    onSortingChange: setSorting,
    onColumnFiltersChange: setColumnFilters,
    getCoreRowModel: getCoreRowModel(),
    onColumnVisibilityChange: setColumnVisibility,
    onRowSelectionChange: setRowSelection,
    manualPagination: true,
    manualSorting: true,
    manualFiltering: true,
    pageCount: pagination.total_pages,
    state: {
      sorting,
      columnFilters,
      columnVisibility,
      rowSelection,
    },
  })

  // Selection helpers for the Gmail-style "select all that match" behaviour.
  const pageSelectedCount = Object.keys(rowSelection).length
  const pageAllSelected = data.length > 0 && pageSelectedCount === data.length
  const hasMoreMatches = pagination.total > data.length
  // Effective number of proxies the bulk actions will operate on.
  const selectedCount = selectAllMatching ? pagination.total : pageSelectedCount

  // The list filters currently applied, shared by every filter-based bulk op.
  const currentFilter = React.useCallback((): ProxyFilter => ({
    search: debouncedSearchQuery || undefined,
    status: statusFilter === "all" ? undefined : statusFilter,
    protocol: protocolFilter === "all" ? undefined : protocolFilter,
  }), [debouncedSearchQuery, statusFilter, protocolFilter])

  // "Select all matching" only makes sense while the whole page is selected; if
  // the user deselects a row, fall back to the explicit selection.
  React.useEffect(() => {
    if (selectAllMatching && !pageAllSelected) {
      setSelectAllMatching(false)
    }
  }, [selectAllMatching, pageAllSelected])

  // Reset the cross-page selection whenever the result set changes underneath
  // it, so it can never apply to a filter/page the user is no longer viewing.
  React.useEffect(() => {
    setSelectAllMatching(false)
  }, [debouncedSearchQuery, statusFilter, protocolFilter, pagination.page, pagination.limit])

  if (isLoading && data.length === 0) {
    return (
      <div className="flex items-center justify-center h-96">
        <Loader2 className="h-8 w-8 animate-spin" />
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">Proxy Management</h1>
          <p className="text-muted-foreground">
            Manage and monitor your proxy infrastructure
          </p>
        </div>
      </div>

      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle>Proxies</CardTitle>
              <CardDescription>
                {pagination.total} total proxies
              </CardDescription>
            </div>
            <div className="flex items-center gap-2">
              <Button onClick={() => setIsAddDialogOpen(true)}>
                <Plus className="mr-2 h-4 w-4" />
                Add Proxy
              </Button>
              <Button
                variant="outline"
                onClick={handleReloadProxies}
                disabled={isReloading}
              >
                <Loader2 className={`mr-2 h-4 w-4 ${isReloading ? 'animate-spin' : ''}`} />
                Reload Pool
              </Button>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="outline">
                    Bulk Actions
                    <ChevronDown className="ml-2 h-4 w-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => setIsImportDialogOpen(true)}>
                    <Upload className="mr-2 h-4 w-4" />
                    Import from TXT
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem onClick={() => handleExport("txt")}>
                    <Download className="mr-2 h-4 w-4" />
                    Export as TXT
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => handleExport("json")}>
                    <Download className="mr-2 h-4 w-4" />
                    Export as JSON
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => handleExport("csv")}>
                    <Download className="mr-2 h-4 w-4" />
                    Export as CSV
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem
                    onClick={handleBulkTest}
                    disabled={selectedCount === 0 || isBulkTesting}
                  >
                    {isBulkTesting ? (
                      <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                    ) : (
                      <Activity className="mr-2 h-4 w-4" />
                    )}
                    Test selected ({selectedCount.toLocaleString()})
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem
                    className="text-red-600"
                    onClick={handleBulkDelete}
                    disabled={selectedCount === 0}
                  >
                    <Trash2 className="mr-2 h-4 w-4" />
                    Delete selected ({selectedCount.toLocaleString()})
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem
                    className="text-red-600 font-semibold"
                    onClick={() => setDeleteAllConfirm(true)}
                  >
                    <Trash2 className="mr-2 h-4 w-4" />
                    Delete ALL proxies
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          </div>
        </CardHeader>
        <CardContent>
          <div className="space-y-4">
            <div className="flex items-center gap-2">
              <Input
                placeholder="Search by address..."
                value={searchQuery}
                onChange={(event) => setSearchQuery(event.target.value)}
                className="max-w-sm"
              />
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="outline" size="icon" className="relative">
                    <Filter className="h-4 w-4" />
                    {(statusFilter !== "all" || protocolFilter !== "all") && (
                      <span className="absolute -top-1 -right-1 h-3 w-3 rounded-full bg-primary" />
                    )}
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end" className="w-56">
                  <DropdownMenuLabel>Filters</DropdownMenuLabel>
                  <DropdownMenuSeparator />
                  <div className="px-2 py-2">
                    <Label className="text-xs text-muted-foreground mb-2 block">Status</Label>
                    <Select
                      value={statusFilter}
                      onValueChange={(value) => {
                        setStatusFilter(value)
                        setPagination(prev => ({ ...prev, page: 1 }))
                      }}
                    >
                      <SelectTrigger className="w-full">
                        <SelectValue placeholder="All statuses" />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="all">All statuses</SelectItem>
                        <SelectItem value="active">Active</SelectItem>
                        <SelectItem value="failed">Failed</SelectItem>
                        <SelectItem value="idle">Idle</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <DropdownMenuSeparator />
                  <div className="px-2 py-2">
                    <Label className="text-xs text-muted-foreground mb-2 block">Protocol</Label>
                    <Select
                      value={protocolFilter}
                      onValueChange={(value) => {
                        setProtocolFilter(value)
                        setPagination(prev => ({ ...prev, page: 1 }))
                      }}
                    >
                      <SelectTrigger className="w-full">
                        <SelectValue placeholder="All protocols" />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="all">All protocols</SelectItem>
                        <SelectItem value="http">HTTP</SelectItem>
                        <SelectItem value="https">HTTPS</SelectItem>
                        <SelectItem value="socks4">SOCKS4</SelectItem>
                        <SelectItem value="socks4a">SOCKS4A</SelectItem>
                        <SelectItem value="socks5">SOCKS5</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                </DropdownMenuContent>
              </DropdownMenu>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="outline">
                    Columns <ChevronDown className="ml-2 h-4 w-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  {table
                    .getAllColumns()
                    .filter((column) => column.getCanHide())
                    .map((column) => {
                      return (
                        <DropdownMenuCheckboxItem
                          key={column.id}
                          className="capitalize"
                          checked={column.getIsVisible()}
                          onCheckedChange={(value) =>
                            column.toggleVisibility(!!value)
                          }
                        >
                          {column.id}
                        </DropdownMenuCheckboxItem>
                      )
                    })}
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
            {pageAllSelected && hasMoreMatches && (
              <div className="flex flex-wrap items-center justify-center gap-x-2 gap-y-1 rounded-md border bg-muted/50 px-4 py-2 text-sm">
                {selectAllMatching ? (
                  <>
                    <span>
                      All <strong>{pagination.total.toLocaleString()}</strong> proxies that match this search are selected.
                    </span>
                    <Button
                      variant="link"
                      className="h-auto p-0"
                      onClick={() => {
                        setSelectAllMatching(false)
                        setRowSelection({})
                      }}
                    >
                      Clear selection
                    </Button>
                  </>
                ) : (
                  <>
                    <span>
                      All <strong>{data.length}</strong> proxies on this page are selected.
                    </span>
                    <Button
                      variant="link"
                      className="h-auto p-0"
                      onClick={() => setSelectAllMatching(true)}
                    >
                      Select all {pagination.total.toLocaleString()} proxies that match this search
                    </Button>
                  </>
                )}
              </div>
            )}
            <div className="rounded-md border">
              <Table>
                <TableHeader>
                  {table.getHeaderGroups().map((headerGroup) => (
                    <TableRow key={headerGroup.id}>
                      {headerGroup.headers.map((header) => {
                        return (
                          <TableHead key={header.id}>
                            {header.isPlaceholder
                              ? null
                              : flexRender(
                                  header.column.columnDef.header,
                                  header.getContext()
                                )}
                          </TableHead>
                        )
                      })}
                    </TableRow>
                  ))}
                </TableHeader>
                <TableBody>
                  {table.getRowModel().rows?.length ? (
                    table.getRowModel().rows.map((row) => (
                      <TableRow
                        key={row.id}
                        data-state={row.getIsSelected() && "selected"}
                      >
                        {row.getVisibleCells().map((cell) => (
                          <TableCell key={cell.id}>
                            {flexRender(
                              cell.column.columnDef.cell,
                              cell.getContext()
                            )}
                          </TableCell>
                        ))}
                      </TableRow>
                    ))
                  ) : (
                    <TableRow>
                      <TableCell
                        colSpan={columns.length}
                        className="h-24 text-center"
                      >
                        No results.
                      </TableCell>
                    </TableRow>
                  )}
                </TableBody>
              </Table>
            </div>
            <div className="flex items-center justify-between space-x-2">
              <div className="flex-1 text-sm text-muted-foreground">
                {selectAllMatching
                  ? `All ${pagination.total.toLocaleString()} matching proxies selected.`
                  : `${pageSelectedCount} of ${data.length} row(s) selected.`}
              </div>
              <div className="flex items-center gap-4">
                <div className="text-sm text-muted-foreground">
                  Page {pagination.page} of {pagination.total_pages} ({pagination.total} total proxies)
                </div>
                <div className="flex items-center space-x-2">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setPagination(prev => ({ ...prev, page: prev.page - 1 }))}
                    disabled={pagination.page <= 1}
                  >
                    Previous
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setPagination(prev => ({ ...prev, page: prev.page + 1 }))}
                    disabled={pagination.page >= pagination.total_pages}
                  >
                    Next
                  </Button>
                </div>
              </div>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Add Proxy Dialog */}
      <Dialog open={isAddDialogOpen} onOpenChange={setIsAddDialogOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add New Proxy</DialogTitle>
            <DialogDescription>
              Add a new proxy to your pool
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-4">
            <div className="grid gap-2">
              <Label htmlFor="address">Address</Label>
              <Input
                id="address"
                placeholder="192.168.1.100:8001"
                className="font-mono"
                value={newProxy.address}
                onChange={(e) => setNewProxy({ ...newProxy, address: e.target.value })}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="protocol">Protocol</Label>
              <Select
                value={newProxy.protocol}
                onValueChange={(value: any) => setNewProxy({ ...newProxy, protocol: value })}
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="http">HTTP</SelectItem>
                  <SelectItem value="https">HTTPS</SelectItem>
                  <SelectItem value="socks4">SOCKS4</SelectItem>
                  <SelectItem value="socks4a">SOCKS4A</SelectItem>
                  <SelectItem value="socks5">SOCKS5</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="username">Username (optional)</Label>
              <Input
                id="username"
                value={newProxy.username}
                onChange={(e) => setNewProxy({ ...newProxy, username: e.target.value })}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="password">Password (optional)</Label>
              <Input
                id="password"
                type="password"
                value={newProxy.password}
                onChange={(e) => setNewProxy({ ...newProxy, password: e.target.value })}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setIsAddDialogOpen(false)}>
              Cancel
            </Button>
            <Button onClick={handleAddProxy}>
              Add Proxy
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Edit Proxy Dialog */}
      <Dialog open={isEditDialogOpen} onOpenChange={setIsEditDialogOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Edit Proxy</DialogTitle>
            <DialogDescription>
              Update proxy configuration
            </DialogDescription>
          </DialogHeader>
          {editingProxy && (
            <div className="grid gap-4 py-4">
              <div className="grid gap-2">
                <Label htmlFor="edit-address">Address</Label>
                <Input
                  id="edit-address"
                  placeholder="192.168.1.100:8001"
                  className="font-mono"
                  value={editingProxy.address}
                  onChange={(e) => setEditingProxy({ ...editingProxy, address: e.target.value })}
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="edit-protocol">Protocol</Label>
                <Select
                  value={editingProxy.protocol}
                  onValueChange={(value: any) => setEditingProxy({ ...editingProxy, protocol: value })}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="http">HTTP</SelectItem>
                    <SelectItem value="https">HTTPS</SelectItem>
                    <SelectItem value="socks4">SOCKS4</SelectItem>
                    <SelectItem value="socks4a">SOCKS4A</SelectItem>
                    <SelectItem value="socks5">SOCKS5</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="edit-username">Username (optional)</Label>
                <Input
                  id="edit-username"
                  value={editingProxy.username || ""}
                  onChange={(e) => setEditingProxy({ ...editingProxy, username: e.target.value })}
                />
              </div>
            </div>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setIsEditDialogOpen(false)}>
              Cancel
            </Button>
            <Button onClick={handleEditProxy}>
              Save Changes
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Import Proxies Dialog */}
      <Dialog open={isImportDialogOpen} onOpenChange={(open) => {
        setIsImportDialogOpen(open)
        if (!open) resetImportDialog()
      }}>
        <DialogContent className="max-w-2xl max-h-[90vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Import Proxies from TXT</DialogTitle>
            <DialogDescription>
              Upload a .txt file with one proxy URL per line. Protocol and
              credentials are read from each URL, e.g.
              {" "}<code>http://user:pass@host:port</code>.
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4 py-4">
            {!importFile ? (
              // File Upload Area
              <div
                className={`border-2 border-dashed rounded-lg p-8 text-center cursor-pointer transition-colors ${
                  isDragging
                    ? 'border-primary bg-primary/5'
                    : 'border-muted-foreground/25 hover:border-primary/50'
                }`}
                onDragOver={handleDragOver}
                onDragLeave={handleDragLeave}
                onDrop={handleDrop}
                onClick={() => {
                  const input = document.createElement('input')
                  input.type = 'file'
                  input.accept = '.txt'
                  input.onchange = (e) => {
                    const file = (e.target as HTMLInputElement).files?.[0]
                    if (file) handleFileUpload(file)
                  }
                  input.click()
                }}
              >
                <FileText className="mx-auto h-12 w-12 text-muted-foreground mb-4" />
                <p className="text-lg font-medium mb-2">
                  Drop your .txt file here or click to browse
                </p>
                <p className="text-sm text-muted-foreground">
                  One proxy URL per line (e.g. protocol://user:pass@host:port)
                </p>
              </div>
            ) : (
              // Preview and Configuration
              <>
                <div className="flex items-center justify-between p-4 bg-muted rounded-lg">
                  <div className="flex items-center gap-3">
                    <FileText className="h-5 w-5 text-primary" />
                    <div>
                      <p className="font-medium">{importFile.name}</p>
                      <p className="text-sm text-muted-foreground">
                        {parsedProxies.length} valid proxies found
                      </p>
                    </div>
                  </div>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => {
                      setImportFile(null)
                      setParsedProxies([])
                    }}
                    disabled={isImporting}
                  >
                    Change File
                  </Button>
                </div>

                {parsedProxies.length > 0 && (
                  <>
                    <div className="grid gap-2">
                      <Label>Preview (first 10 proxies)</Label>
                      <div className="border rounded-md p-3 bg-muted/30 max-h-32 overflow-y-auto">
                        <div className="font-mono text-sm space-y-1">
                          {parsedProxies.slice(0, 10).map((proxy, idx) => (
                            <div key={idx} className="text-muted-foreground">
                              {proxy.protocol}://
                              {proxy.username ? `${proxy.username}:***@` : ""}
                              {proxy.address}
                            </div>
                          ))}
                          {parsedProxies.length > 10 && (
                            <div className="text-xs text-muted-foreground pt-1">
                              ... and {parsedProxies.length - 10} more
                            </div>
                          )}
                        </div>
                      </div>
                    </div>

                    {isImporting && (
                      <div className="space-y-3">
                        <div className="flex items-center justify-between text-sm">
                          <span className="text-muted-foreground">
                            Progress: {importProgress.current} / {importProgress.total}
                          </span>
                          <div className="flex gap-3 text-muted-foreground">
                            <span>
                              <span className="text-green-600 font-medium">{importProgress.success}</span> success
                            </span>
                            {importProgress.skipped > 0 && (
                              <span>
                                <span className="text-yellow-600 font-medium">{importProgress.skipped}</span> skipped
                              </span>
                            )}
                            {importProgress.failed > 0 && (
                              <span>
                                <span className="text-red-600 font-medium">{importProgress.failed}</span> failed
                              </span>
                            )}
                          </div>
                        </div>
                        <div className="w-full bg-secondary rounded-full h-2.5">
                          <div
                            className="bg-primary h-2.5 rounded-full transition-all duration-300"
                            style={{
                              width: `${(importProgress.current / importProgress.total) * 100}%`
                            }}
                          />
                        </div>
                      </div>
                    )}

                    {importProgress.current === importProgress.total && importProgress.total > 0 && (
                      <div className="border rounded-lg p-4 space-y-3 bg-muted/30">
                        <div className="flex items-center justify-between">
                          <h4 className="font-medium">Import Complete</h4>
                          <div className="flex gap-4 text-sm">
                            <span className="flex items-center gap-1 text-green-600">
                              <CheckCircle2 className="h-4 w-4" />
                              {importProgress.success} successful
                            </span>
                            {importProgress.skipped > 0 && (
                              <span className="flex items-center gap-1 text-yellow-600">
                                <AlertCircle className="h-4 w-4" />
                                {importProgress.skipped} skipped
                              </span>
                            )}
                            {importProgress.failed > 0 && (
                              <span className="flex items-center gap-1 text-red-600">
                                <XCircle className="h-4 w-4" />
                                {importProgress.failed} failed
                              </span>
                            )}
                          </div>
                        </div>

                        {(importResults.filter(r => r.status === 'skipped').length > 0 ||
                          importResults.filter(r => r.status === 'failed').length > 0) && (
                          <div className="max-h-48 overflow-y-auto text-sm space-y-2">
                            {importResults.filter(r => r.status === 'skipped').length > 0 && (
                              <div>
                                <p className="font-medium text-yellow-600 mb-1">Skipped proxies (duplicates):</p>
                                <div className="space-y-0.5">
                                  {importResults
                                    .filter(r => r.status === 'skipped')
                                    .map((result, idx) => (
                                      <div key={idx} className="font-mono text-xs text-yellow-600/80">
                                        {result.address}
                                      </div>
                                    ))}
                                </div>
                              </div>
                            )}
                            {importResults.filter(r => r.status === 'failed').length > 0 && (
                              <div>
                                <p className="font-medium text-red-600 mb-1">Failed proxies:</p>
                                <div className="space-y-0.5">
                                  {importResults
                                    .filter(r => r.status === 'failed')
                                    .map((result, idx) => (
                                      <div key={idx} className="font-mono text-xs text-red-600">
                                        {result.address}: {result.error}
                                      </div>
                                    ))}
                                </div>
                              </div>
                            )}
                          </div>
                        )}
                      </div>
                    )}
                  </>
                )}
              </>
            )}
          </div>

          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => {
                setIsImportDialogOpen(false)
                resetImportDialog()
              }}
              disabled={isImporting}
            >
              {importProgress.current === importProgress.total && importProgress.total > 0 ? 'Close' : 'Cancel'}
            </Button>
            {importFile && parsedProxies.length > 0 && (
              <Button
                onClick={handleImport}
                disabled={isImporting || (importProgress.current === importProgress.total && importProgress.total > 0)}
              >
                {isImporting ? (
                  <>
                    <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                    Importing...
                  </>
                ) : importProgress.current === importProgress.total && importProgress.total > 0 ? (
                  'Import Complete'
                ) : (
                  `Import ${parsedProxies.length} Proxies`
                )}
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete Confirmation Dialog */}
      <AlertDialog open={deleteConfirm.open} onOpenChange={(open) => setDeleteConfirm({ open, proxyId: null })}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Are you sure?</AlertDialogTitle>
            <AlertDialogDescription>
              This action cannot be undone. This will permanently delete the proxy.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={confirmDelete} className="bg-red-600 hover:bg-red-700">
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Bulk Delete Confirmation Dialog */}
       <AlertDialog open={bulkDeleteConfirm} onOpenChange={setBulkDeleteConfirm}>
         <AlertDialogContent>
           <AlertDialogHeader>
             <AlertDialogTitle>Delete {selectedCount.toLocaleString()} proxies?</AlertDialogTitle>
             <AlertDialogDescription>
               This action cannot be undone. This will permanently delete{" "}
               {selectAllMatching
                 ? "every proxy that matches the current search and filters"
                 : "the selected proxies"}.
             </AlertDialogDescription>
           </AlertDialogHeader>
           <AlertDialogFooter>
             <AlertDialogCancel>Cancel</AlertDialogCancel>
             <AlertDialogAction onClick={confirmBulkDelete} className="bg-red-600 hover:bg-red-700">
               Delete
             </AlertDialogAction>
           </AlertDialogFooter>
         </AlertDialogContent>
       </AlertDialog>

       <AlertDialog open={deleteAllConfirm} onOpenChange={setDeleteAllConfirm}>
         <AlertDialogContent>
           <AlertDialogHeader>
             <AlertDialogTitle>Delete ALL proxies?</AlertDialogTitle>
             <AlertDialogDescription>
               This will permanently delete <strong>every proxy</strong> in the database,
               including those in pools. This action cannot be undone.
             </AlertDialogDescription>
           </AlertDialogHeader>
           <AlertDialogFooter>
             <AlertDialogCancel>Cancel</AlertDialogCancel>
             <AlertDialogAction onClick={confirmDeleteAll} className="bg-red-600 hover:bg-red-700">
               Delete All
             </AlertDialogAction>
           </AlertDialogFooter>
         </AlertDialogContent>
       </AlertDialog>
    </div>
  )
}
