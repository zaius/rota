// API Response Types

// Protocol is the single source of truth for supported proxy protocols; the
// same union used to be written out inline in half a dozen places.
export type Protocol = "http" | "https" | "socks4" | "socks4a" | "socks5"
export const PROTOCOLS: Protocol[] = ["http", "https", "socks4", "socks4a", "socks5"]

export interface Proxy {
  id: number
  address: string
  protocol: Protocol
  status: "active" | "failed" | "idle"
  requests: number
  success_rate: number
  avg_response_time: number
  last_check: string
  cooldown_until?: string
  cooldown_reason?: string
  username?: string
  tags: string[]
  source_id?: number        // absent for proxies added by hand
  created_at: string
  updated_at: string
}

export interface ProxiesResponse {
  proxies: Proxy[]
  pagination: {
    page: number
    limit: number
    total: number
    total_pages: number
  }
}

export interface DashboardStats {
  active_proxies: number
  total_proxies: number
  total_requests: number
  avg_success_rate: number
  avg_response_time: number
  request_growth: number
  success_rate_growth: number
  response_time_delta: number
  tunnels: TunnelStats
}

/**
 * CONNECT tunnel activity. HTTPS traffic does not show up in the request
 * counters above: one tunnel is one event however many requests the client
 * sends through it, so bytes and mean concurrency are the volume signal.
 */
export interface TunnelStats {
  open: number
  today: number
  bytes_up_today: number
  bytes_down_today: number
  mean_concurrency: number
  /** Requests seen inside intercepted tunnels; 0 when interception is off. */
  requests_today: number
}

export type ChartRange = "1h" | "6h" | "24h" | "7d" | "30d"

export interface TrafficPoint {
  time: string // bucket start, RFC3339
  requests: number
  successes: number
  p50_ms: number
  p95_ms: number
}

export interface TrafficChartResponse {
  range: ChartRange
  bucket_seconds: number
  data: TrafficPoint[]
}

export interface DomainStats {
  domain: string
  requests: number
  successes: number
  failures: number
  rate_limited: number
  avg_response_time: number
  p50_ms: number
  p95_ms: number
  tunnels: number
  tunnel_errors: number
  bytes_up: number
  bytes_down: number
}

export interface DomainStatsResponse {
  range: ChartRange
  data: DomainStats[]
}

export interface Settings {
  rotation: {
    follow_redirect: boolean
    timeout: number
    session_ttl_minutes: number
  }
  healthcheck: {
    interval_minutes: number
    timeout: number
    workers: number
    url: string
    status: number
    headers: string[]
    strict_tls: boolean
  }
  proxy_cleanup: {
    enabled: boolean
    max_failed_days: number
    min_success_rate: number
    cleanup_interval_hours: number
  }
}

export interface ApiError {
  error: string
  details?: string
}

// Request Types
export interface AddProxyRequest {
  address: string
  protocol: Protocol
  username?: string
  password?: string
  tags?: string[]
}

// Omitted credentials and tags keep their stored values; null or "" clears them.
export interface UpdateProxyRequest {
  address?: string
  protocol?: Protocol
  username?: string | null
  password?: string | null
  tags?: string[]
}

// Narrows a bulk operation to the proxies matching the current list filters.
// An empty/omitted filter matches every proxy.
export interface ProxyFilter {
  search?: string
  status?: string
  protocol?: string
  source?: string           // a source ID, or "manual"
}

export interface BulkDeleteRequest {
  ids?: number[]
  all?: boolean
  filter?: ProxyFilter
}

export interface BulkTagRequest {
  ids?: number[]
  all?: boolean
  filter?: ProxyFilter
  add?: string[]
  remove?: string[]
}

export interface BulkTestRequest {
  ids?: number[]
  all?: boolean
  filter?: ProxyFilter
}

export interface ProxyTestResult {
  id: number
  address: string
  status: "active" | "failed"
  response_time?: number
  error?: string
  tested_at: string
  duration?: number // Alias for response_time for better clarity
}

// ── Proxy Sources ──────────────────────────────────────────────────────────
// Line format of the source file: a lineformat template like
// "host:port:user:pass" or "[protocol://][user[:pass]@]host:port" — see
// lib/lineformat.ts.
export type SourceFormat = string

// A custom line format used before, offered for re-picking in the dashboard.
export interface FormatHistoryEntry {
  id: number
  format: string
  use_count: number
  last_used_at: string
}

export interface ProxySource {
  id: number
  name: string
  url: string
  protocol: Protocol
  format: SourceFormat
  enabled: boolean
  interval_minutes: number
  last_fetched_at?: string
  last_count: number        // newly imported on last fetch
  last_total: number        // total lines returned on last fetch
  last_error?: string
  cleanup_enabled: boolean
  cleanup_days: number
  proxy_count: number       // proxies currently attributed to this source
  created_at: string
  updated_at: string
}

export interface CreateSourceRequest {
  name: string
  url: string
  protocol: Protocol
  format?: SourceFormat
  enabled: boolean
  interval_minutes: number
  cleanup_enabled?: boolean
  cleanup_days?: number
}

export interface UpdateSourceRequest {
  name?: string
  url?: string
  protocol?: string
  format?: SourceFormat
  enabled?: boolean
  interval_minutes?: number
  cleanup_enabled?: boolean
  cleanup_days?: number
}

export type JobStatus = "pending" | "running" | "done" | "failed"

// Session is one live sticky-session binding: a token holding a proxy in a scope.
export interface Session {
  token: string
  scope: string
  proxy_id: number
  created_at: string
  last_used: string
  expires_at: string
}

// Job is the shape the bulk proxy test returns and the UI polls.
export interface Job {
  id: string
  status: JobStatus
  progress: number
  total: number
  active: number
  failed: number
  skipped?: number
  error?: string
  started_at: string
  updated_at: string
  finished_at?: string
  results?: ProxyTestResult[]
}
