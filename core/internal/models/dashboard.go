package models

import "time"

// DashboardStats represents dashboard statistics
type DashboardStats struct {
	ActiveProxies     int     `json:"active_proxies"`
	TotalProxies      int     `json:"total_proxies"`
	TotalRequests     int64   `json:"total_requests"`
	AvgSuccessRate    float64 `json:"avg_success_rate"`
	AvgResponseTime   int     `json:"avg_response_time"`
	RequestGrowth     float64 `json:"request_growth"`
	SuccessRateGrowth float64 `json:"success_rate_growth"`
	ResponseTimeDelta int     `json:"response_time_delta"`
}

// ChartDataPoint represents a single data point in a chart
type ChartDataPoint struct {
	Time  string `json:"time"`
	Value int    `json:"value"`
}

// SuccessRateDataPoint represents a data point for success rate chart
type SuccessRateDataPoint struct {
	Time    string `json:"time"`
	Success int    `json:"success"`
	Failure int    `json:"failure"`
}

// TrafficPoint is one time bucket of the traffic series: request volume plus
// latency percentiles of the successful requests in the bucket. P50Ms/P95Ms
// are 0 when the bucket has no successful requests.
type TrafficPoint struct {
	Time      time.Time `json:"time"` // bucket start, RFC3339
	Requests  int64     `json:"requests"`
	Successes int64     `json:"successes"`
	P50Ms     int       `json:"p50_ms"`
	P95Ms     int       `json:"p95_ms"`
}

// TrafficChartData is the traffic-series chart payload. BucketSeconds tells
// the client the bucket width without having to re-derive the range mapping.
type TrafficChartData struct {
	Range         string         `json:"range"`
	BucketSeconds int            `json:"bucket_seconds"`
	Data          []TrafficPoint `json:"data"`
}

// ResponseTimeChartData represents response time chart data
type ResponseTimeChartData struct {
	Data []ChartDataPoint `json:"data"`
}

// SuccessRateChartData represents success rate chart data
type SuccessRateChartData struct {
	Data []SuccessRateDataPoint `json:"data"`
}

// ProxyStatusSimple represents simplified proxy status for dashboard
type ProxyStatusSimple struct {
	ID          string  `json:"id"`
	Address     string  `json:"address"`
	Status      string  `json:"status"`
	Requests    int64   `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

// ProxyStatusList represents a list of simplified proxy statuses
type ProxyStatusList struct {
	Proxies []ProxyStatusSimple `json:"proxies"`
}
