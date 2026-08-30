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

export interface ChartDataPoint {
  time: string
  value?: number
  success?: number
  failure?: number
}

export interface ChartResponse {
  data: ChartDataPoint[]
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

export interface LogEntry {
  id: string
  timestamp: string
  level: "info" | "warning" | "error" | "success"
  message: string
  details?: string
  metadata?: Record<string, any>
}

export interface LogsResponse {
  logs: LogEntry[]
  pagination: {
    page: number
    limit: number
    total: number
    total_pages: number
  }
}

export interface Settings {
  rotation: {
    follow_redirect: boolean
    timeout: number
  }
  rate_limit: {
    enabled: boolean
    interval: number
    max_requests: number
  }
  healthcheck: {
    timeout: number
    workers: number
    url: string
    status: number
    headers: string[]
  }
  log_retention: {
    enabled: boolean
    retention_days: number
    compression_after_days: number
    cleanup_interval_hours: number
  }
}

export interface AuthResponse {
  token: string
  user: {
    username: string
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
}

export interface UpdateProxyRequest {
  address?: string
  protocol?: Protocol
  username?: string
  password?: string
}

export interface BulkProxyRequest {
  proxies: AddProxyRequest[]
}

// Narrows a bulk operation to the proxies matching the current list filters.
// An empty/omitted filter matches every proxy.
export interface ProxyFilter {
  search?: string
  status?: string
  protocol?: string
}

export interface BulkDeleteRequest {
  ids?: number[]
  all?: boolean
  filter?: ProxyFilter
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

// ── Proxy Pools ────────────────────────────────────────────────────────────
export interface ProxyPool {
  id: number
  name: string
  description: string
  country_code?: string
  region_name?: string
  city_name?: string
  rotation_method: "roundrobin" | "random" | "stick" | "session"
  stick_count: number
  session_ttl_minutes: number
  health_check_url: string
  health_check_cron: string
  health_check_enabled: boolean
  auto_sync: boolean
  sync_mode: "auto" | "manual"
  enabled: boolean
  total_proxies: number
  active_proxies: number
  failed_proxies: number
  geo_filters?: GeoFilter[]
  isp_filters?: string[]
  tag_filters?: string[]
  created_at: string
  updated_at: string
}

export interface PoolAlertRule {
  id: number
  pool_id: number
  enabled: boolean
  min_active_proxies: number
  webhook_url: string
  webhook_method: "POST" | "GET"
  last_fired_at?: string
  cooldown_minutes: number
  created_at: string
  updated_at: string
}

export interface CreatePoolAlertRuleRequest {
  enabled: boolean
  min_active_proxies: number
  webhook_url: string
  webhook_method?: "POST" | "GET"
  cooldown_minutes?: number
}

export interface PoolProxy {
  proxy_id: number
  address: string
  protocol: string
  status: string
  country_code?: string
  country_name?: string
  region_name?: string
  city_name?: string
  isp?: string
  requests: number
  success_rate: number
  avg_response_time: number
  last_check?: string
  added_at: string
}

export interface GeoSummaryItem {
  country_code: string
  country_name: string
  region_name: string
  city_name: string
  total: number
  active: number
}

export interface GeoCityItem {
  city_name: string
  region_name: string
  total: number
  active: number
}

export interface GeoFilter {
  country_code: string
  city_name?: string
}

export type JobStatus = "pending" | "running" | "done" | "failed"
export type JobKind = "pool_health_check" | "bulk_test"

// Job is the shape returned by the async job store, shared by pool health
// checks and bulk proxy tests. Kind-specific fields are optional.
export interface Job {
  id: string
  kind?: JobKind
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
  // Pool health-check specific
  pool_id?: number
  pool_name?: string
  check_url?: string
  workers?: number
}

// ── Proxy Users ────────────────────────────────────────────────────────────

/**
 * Client TLS/HTTP2 fingerprint Rota presents to the target server for a user
 * whose traffic is intercepted. "" is the stored default and behaves as "go".
 * Must stay in sync with models.TLSProfiles in the core.
 */
export type TLSProfile =
  | ""
  | "go"
  | "ios"
  | "ios-18"
  | "android"
  | "android-okhttp"
  | "chrome"
  | "firefox"

/** Profile names with their labels, in the order the dropdown shows them. */
export const TLS_PROFILES: { value: TLSProfile; label: string }[] = [
  { value: "go", label: "Go (no impersonation)" },
  { value: "ios", label: "Safari on iOS 26 (iPhone)" },
  { value: "ios-18", label: "Safari on iOS 18.5 (iPhone)" },
  { value: "android", label: "Chrome on Android (phone browser)" },
  { value: "android-okhttp", label: "OkHttp 4 on Android 13 (native app)" },
  { value: "chrome", label: "Chrome 146 (desktop)" },
  { value: "firefox", label: "Firefox 148 (desktop)" },
]

export interface ProxyUser {
  id: number
  username: string
  enabled: boolean
  main_pool_id?: number
  main_pool_name?: string
  fallback_pool_ids: number[]
  max_retries: number
  requests_per_minute: number
  inspect_tls: boolean
  tls_profile: TLSProfile
  created_at: string
  updated_at: string
}

export interface CreateProxyUserRequest {
  username: string
  password: string
  enabled: boolean
  main_pool_id?: number | null
  fallback_pool_ids: number[]
  max_retries: number
  requests_per_minute?: number
  inspect_tls?: boolean
  tls_profile?: TLSProfile
}

export interface UpdateProxyUserRequest {
  password?: string
  enabled?: boolean
  main_pool_id?: number | null
  fallback_pool_ids?: number[]
  max_retries?: number
  requests_per_minute?: number
  inspect_tls?: boolean
  tls_profile?: TLSProfile
}

export interface CreatePoolRequest {
  name: string
  description?: string
  country_code?: string
  region_name?: string
  city_name?: string
  geo_filters?: GeoFilter[]
  isp_filters?: string[]
  tag_filters?: string[]
  rotation_method: "roundrobin" | "random" | "stick" | "session"
  stick_count: number
  session_ttl_minutes?: number
  health_check_url?: string
  health_check_cron?: string
  health_check_enabled: boolean
  auto_sync: boolean
  sync_mode?: "auto" | "manual"
  enabled: boolean
}
