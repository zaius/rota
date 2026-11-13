import {
  Proxy,
  ProxiesResponse,
  DashboardStats,
  DomainStatsResponse,
  ChartResponse,
  TrafficChartResponse,
  Settings,
  AddProxyRequest,
  UpdateProxyRequest,
  BulkProxyRequest,
  BulkDeleteRequest,
  BulkTagRequest,
  BulkTestRequest,
  ProxyFilter,
  ProxyTestResult,
  ProxySource,
  CreateSourceRequest,
  UpdateSourceRequest,
  FormatHistoryEntry,
  Job,
  JobStatus,
} from "./types"

// Empty base = same origin: in production the Go server serves both the SPA and
// the API, and in dev the Vite proxy (see vite.config.ts) forwards /api and /ws
// to the core. Set VITE_API_URL only to point at a different API host.
const API_BASE_URL = import.meta.env.VITE_API_URL || ""

class ApiClient {
  private baseUrl: string
  private token: string | null = null

  constructor(baseUrl: string = API_BASE_URL) {
    this.baseUrl = baseUrl
    // Load token from localStorage if available
    if (typeof window !== "undefined") {
      this.token = localStorage.getItem("auth_token")
    }
  }

  setToken(token: string) {
    this.token = token
    if (typeof window !== "undefined") {
      localStorage.setItem("auth_token", token)
    }
  }

  clearToken() {
    this.token = null
    if (typeof window !== "undefined") {
      localStorage.removeItem("auth_token")
    }
  }

  private getHeaders(): HeadersInit {
    const headers: HeadersInit = {
      "Content-Type": "application/json",
    }
    if (this.token) {
      headers["Authorization"] = `Bearer ${this.token}`
    }
    return headers
  }

  private async request<T>(
    endpoint: string,
    options: RequestInit = {}
  ): Promise<T> {
    const url = `${this.baseUrl}${endpoint}`
    const response = await fetch(url, {
      ...options,
      headers: {
        ...this.getHeaders(),
        ...options.headers,
      },
    })

    if (response.status === 401) {
      this.clearToken()
      if (typeof window !== "undefined") {
        window.location.href = "/login?reason=session_expired"
      }
      throw new Error("Password rejected. Please sign in again.")
    }

    if (!response.ok) {
      const error = await response.json().catch(() => ({
        error: `HTTP ${response.status}: ${response.statusText}`,
      }))
      throw new Error(error.error || error.message || "Request failed")
    }

    // Handle 204 No Content
    if (response.status === 204) {
      return {} as T
    }

    return response.json()
  }

  // Authentication: the shared password is the Bearer token. Checking it here
  // rather than through request() reports a wrong password on the form
  // instead of redirecting back to it.
  async login(password: string): Promise<void> {
    const response = await fetch(`${this.baseUrl}/api/v1/status`, {
      headers: { Authorization: `Bearer ${password}` },
    })
    if (response.status === 401) throw new Error("Wrong password.")
    if (!response.ok) throw new Error(`HTTP ${response.status}: ${response.statusText}`)
    this.setToken(password)
  }

  async checkAuth(): Promise<void> {
    await this.request("/api/v1/status")
  }

  // Dashboard
  async getDashboardStats(): Promise<DashboardStats> {
    return this.request<DashboardStats>("/api/v1/dashboard/stats")
  }

  async getDomainStats(range: string = "24h", domain: string = ""): Promise<DomainStatsResponse> {
    const params = new URLSearchParams({ range, domain })
    return this.request(`/api/v1/dashboard/domains?${params}`)
  }

  async getTrafficChart(range: string = "24h"): Promise<TrafficChartResponse> {
    return this.request<TrafficChartResponse>(
      `/api/v1/dashboard/charts/traffic?range=${range}`
    )
  }

  async getResponseTimeChart(interval: string = "4h"): Promise<ChartResponse> {
    return this.request<ChartResponse>(
      `/api/v1/dashboard/charts/response-time?interval=${interval}`
    )
  }

  async getSuccessRateChart(interval: string = "4h"): Promise<ChartResponse> {
    return this.request<ChartResponse>(
      `/api/v1/dashboard/charts/success-rate?interval=${interval}`
    )
  }

  // Proxies
  async getProxies(params?: {
    page?: number
    limit?: number
    search?: string
    status?: string
    protocol?: string
    sort?: string
    order?: "asc" | "desc"
  }): Promise<ProxiesResponse> {
    const searchParams = new URLSearchParams()
    if (params) {
      Object.entries(params).forEach(([key, value]) => {
        if (value !== undefined) {
          searchParams.append(key, value.toString())
        }
      })
    }
    const query = searchParams.toString()
    return this.request<ProxiesResponse>(
      `/api/v1/proxies${query ? `?${query}` : ""}`
    )
  }

  async addProxy(proxy: AddProxyRequest): Promise<Proxy> {
    return this.request<Proxy>("/api/v1/proxies", {
      method: "POST",
      body: JSON.stringify(proxy),
    })
  }

  async updateProxy(id: number, proxy: UpdateProxyRequest): Promise<Proxy> {
    return this.request<Proxy>(`/api/v1/proxies/${id}`, {
      method: "PUT",
      body: JSON.stringify(proxy),
    })
  }

  async deleteProxy(id: number): Promise<void> {
    return this.request<void>(`/api/v1/proxies/${id}`, {
      method: "DELETE",
    })
  }

  async bulkAddProxies(request: BulkProxyRequest): Promise<{
    created: number
    failed: number
    results: Array<{ address: string; status: string; id?: string }>
  }> {
    return this.request("/api/v1/proxies/bulk", {
      method: "POST",
      body: JSON.stringify(request),
    })
  }

  async bulkDeleteProxies(request: BulkDeleteRequest): Promise<{
    deleted: number
    message: string
  }> {
    return this.request("/api/v1/proxies/bulk-delete", {
      method: "POST",
      body: JSON.stringify(request),
    })
  }

  async bulkTagProxies(request: BulkTagRequest): Promise<{ updated: number; message: string }> {
    return this.request("/api/v1/proxies/bulk-tags", {
      method: "POST",
      body: JSON.stringify(request),
    })
  }

  async deleteAllProxies(): Promise<{ deleted: number }> {
    return this.request("/api/v1/proxies", { method: "DELETE" })
  }

  async testProxy(id: number): Promise<ProxyTestResult> {
    return this.request<ProxyTestResult>(`/api/v1/proxies/${id}/test`, {
      method: "POST",
    })
  }

  async bulkTestProxies(
    request: BulkTestRequest
  ): Promise<{ job_id: string; total: number; status: JobStatus }> {
    return this.request("/api/v1/proxies/bulk-test", {
      method: "POST",
      body: JSON.stringify(request),
    })
  }

  async getBulkTestJob(jobId: string): Promise<Job> {
    return this.request<Job>(`/api/v1/proxies/bulk-test/${jobId}`)
  }

  async getLatestBulkTestJob(): Promise<Job | null> {
    const res = await this.request<{ job: Job | null }>("/api/v1/proxies/bulk-test")
    return res.job
  }

  async invalidateProxy(
    id: number,
    opts?: { minutes?: number; reason?: string; domain?: string }
  ): Promise<{ status: string; id: number; address: string; cooldown_until?: string }> {
    return this.request(`/api/v1/proxies/${id}/invalidate`, {
      method: "POST",
      body: JSON.stringify(opts ?? {}),
    })
  }

  async reactivateProxy(id: number): Promise<{ status: string; id: number }> {
    return this.request(`/api/v1/proxies/${id}/reactivate`, {
      method: "POST",
    })
  }

  async exportProxies(
    format: "txt" | "json" | "csv" = "txt",
    filter?: ProxyFilter
  ): Promise<Blob> {
    const params = new URLSearchParams({ format })
    if (filter?.status) params.append("status", filter.status)
    if (filter?.search) params.append("search", filter.search)
    if (filter?.protocol) params.append("protocol", filter.protocol)

    const response = await fetch(
      `${this.baseUrl}/api/v1/proxies/export?${params.toString()}`,
      {
        headers: this.getHeaders(),
      }
    )

    if (!response.ok) {
      throw new Error("Export failed")
    }

    return response.blob()
  }

  async getTagList(): Promise<string[]> {
    const res = await this.request<{ tags: string[] | null }>("/api/v1/proxies/tags")
    return res.tags ?? []
  }

  async reloadProxies(): Promise<{ status: string; message: string }> {
    return this.request("/api/v1/proxies/reload", {
      method: "POST",
    })
  }

  // Settings
  async getSettings(): Promise<Settings> {
    return this.request<Settings>("/api/v1/settings")
  }

  async updateSettings(settings: Partial<Settings>): Promise<{
    message: string
    config: Settings
  }> {
    return this.request("/api/v1/settings", {
      method: "PUT",
      body: JSON.stringify(settings),
    })
  }

  async resetSettings(): Promise<{
    message: string
    config: Settings
  }> {
    return this.request("/api/v1/settings/reset", {
      method: "POST",
    })
  }

  // ── Proxy Sources ─────────────────────────────────────────────────────────
  async getSources(): Promise<{ sources: ProxySource[] }> {
    return this.request("/api/v1/sources")
  }

  async createSource(req: CreateSourceRequest): Promise<ProxySource> {
    return this.request("/api/v1/sources", { method: "POST", body: JSON.stringify(req) })
  }

  async updateSource(id: number, req: UpdateSourceRequest): Promise<ProxySource> {
    return this.request(`/api/v1/sources/${id}`, { method: "PUT", body: JSON.stringify(req) })
  }

  async deleteSource(id: number): Promise<void> {
    return this.request(`/api/v1/sources/${id}`, { method: "DELETE" })
  }

  async fetchSourceNow(id: number): Promise<{ source: ProxySource; imported: number }> {
    return this.request(`/api/v1/sources/${id}/fetch`, { method: "POST" })
  }

  async enrichGeo(): Promise<{ enriched: number }> {
    return this.request("/api/v1/sources/enrich-geo", { method: "POST" })
  }

  // ── Line-format history ───────────────────────────────────────────────────
  async getFormatHistory(): Promise<{ formats: FormatHistoryEntry[] }> {
    return this.request("/api/v1/format-history")
  }

  async recordFormat(format: string): Promise<{ status: string }> {
    return this.request("/api/v1/format-history", { method: "POST", body: JSON.stringify({ format }) })
  }

  async deleteFormatHistory(id: number): Promise<{ status: string }> {
    return this.request(`/api/v1/format-history/${id}`, { method: "DELETE" })
  }
}

// Export singleton instance
export const api = new ApiClient()

// Export class for custom instances
export { ApiClient }
