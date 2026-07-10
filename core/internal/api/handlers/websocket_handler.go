package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/gorilla/websocket"
)

// WebSocketHandler handles WebSocket connections
type WebSocketHandler struct {
	dashboardRepo  *repository.DashboardRepository
	proxyRepo      *repository.ProxyRepository
	events         events.Store
	logger         *logger.Logger
	allowedOrigins []string
	upgrader       websocket.Upgrader
}

// NewWebSocketHandler creates a new WebSocketHandler. allowedOrigins is the
// configured CORS allowlist, consulted by the origin check on upgrade.
func NewWebSocketHandler(
	dashboardRepo *repository.DashboardRepository,
	proxyRepo *repository.ProxyRepository,
	eventStore events.Store,
	log *logger.Logger,
	allowedOrigins []string,
) *WebSocketHandler {
	h := &WebSocketHandler{
		dashboardRepo:  dashboardRepo,
		proxyRepo:      proxyRepo,
		events:         eventStore,
		logger:         log,
		allowedOrigins: allowedOrigins,
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     h.checkOrigin,
	}
	return h
}

// checkOrigin guards against cross-site WebSocket hijacking. The same-origin
// policy does not apply to WebSocket upgrades, so without this check any page
// the victim visits could open an authenticated socket to the dashboard.
//
// It permits requests with no Origin header (non-browser clients such as CLI
// tools cannot be victims of CSWSH), same-origin requests, and origins present
// in the configured CORS allowlist.
func (h *WebSocketHandler) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, allowed := range h.allowedOrigins {
		if allowed == "*" || strings.EqualFold(allowed, origin) {
			return true
		}
	}
	h.logger.Warn("rejected websocket upgrade from disallowed origin", "origin", origin)
	return false
}

// DashboardWebSocket handles dashboard real-time updates
func (h *WebSocketHandler) DashboardWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("failed to upgrade websocket connection", "error", err)
		return
	}
	defer conn.Close()

	h.logger.Info("dashboard websocket connection established", "remote_addr", r.RemoteAddr)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Send updates every 5 seconds
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Send initial data immediately
	if err := h.sendDashboardUpdate(ctx, conn); err != nil {
		h.logger.Error("failed to send initial dashboard update", "error", err)
		return
	}

	// Handle incoming messages and send periodic updates
	for {
		select {
		case <-ticker.C:
			if err := h.sendDashboardUpdate(ctx, conn); err != nil {
				h.logger.Error("failed to send dashboard update", "error", err)
				return
			}

		case <-ctx.Done():
			h.logger.Info("dashboard websocket context cancelled")
			return
		}

		// Check for client messages (for keep-alive or commands)
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, _, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				h.logger.Warn("dashboard websocket unexpected close", "error", err)
			}
			return
		}
	}
}

// sendDashboardUpdate sends dashboard statistics to the WebSocket client
func (h *WebSocketHandler) sendDashboardUpdate(ctx context.Context, conn *websocket.Conn) error {
	stats, err := h.dashboardRepo.GetStats(ctx)
	if err != nil {
		return err
	}

	message := map[string]interface{}{
		"type": "stats_update",
		"data": stats,
	}

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteJSON(message)
}

// LogsWebSocket handles real-time log streaming
func (h *WebSocketHandler) LogsWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("failed to upgrade websocket connection", "error", err)
		return
	}
	defer conn.Close()

	h.logger.Info("logs websocket connection established", "remote_addr", r.RemoteAddr)

	// Set up ping/pong to keep connection alive
	conn.SetReadDeadline(time.Time{}) // Remove initial read deadline
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		return nil
	})

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Channel for log messages - large buffer to prevent blocking
	logChan := make(chan interface{}, 10000)
	defer close(logChan)

	// Filter settings from client
	var filterLevels []string // empty means all levels
	var filterSource string   // empty means all sources
	filterMutex := &sync.RWMutex{}

	// Goroutine to send messages from channel to WebSocket
	go func() {
		for {
			select {
			case msg := <-logChan:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteJSON(msg); err != nil {
					h.logger.Error("failed to write to websocket", "error", err)
					cancel()
					return
				}
			case <-ctx.Done():
				h.logger.Info("writer goroutine stopped")
				return
			}
		}
	}()

	// Goroutine to read client messages (for filter updates)
	go func() {
		defer h.logger.Info("reader goroutine stopped")
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			// Set a read deadline - will be updated by pong handler
			conn.SetReadDeadline(time.Now().Add(120 * time.Second))
			_, message, err := conn.ReadMessage()
			if err != nil {
				// Timeout means no client message, which is normal
				if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
					continue
				}
				// Check for normal close or unexpected close
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					h.logger.Info("logs websocket closed normally")
					cancel()
					return
				}
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					h.logger.Warn("logs websocket unexpected close", "error", err)
				}
				cancel()
				return
			}

			// Parse filter message from client
			var filterMsg struct {
				Action string   `json:"action"`
				Levels []string `json:"levels"`
				Source string   `json:"source"`
			}
			if err := json.Unmarshal(message, &filterMsg); err == nil {
				if filterMsg.Action == "filter" {
					filterMutex.Lock()
					filterLevels = filterMsg.Levels
					filterSource = filterMsg.Source
					filterMutex.Unlock()
					h.logger.Info("logs filter updated", "levels", filterLevels, "source", filterSource)
				}
			}
		}
	}()

	// Poll for new logs every 2 seconds
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Ping ticker to keep connection alive
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	// Get the current maximum log ID to start streaming from
	// This ensures we only stream new logs from the moment the connection starts
	lastLogID := int64(0)
	currentLogs, _, err := h.events.ListLogs(ctx, events.LogFilter{Source: filterSource}, 1, 1)
	if err == nil && len(currentLogs) > 0 {
		lastLogID = currentLogs[0].ID
		h.logger.Info("starting log stream from current position", "last_log_id", lastLogID)
	}

	for {
		select {
		case <-pingTicker.C:
			// Send ping to keep connection alive
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				h.logger.Error("failed to send ping", "error", err)
				return
			}

		case <-ticker.C:
			filterMutex.RLock()
			currentFilterSource := filterSource
			currentFilterLevels := filterLevels
			filterMutex.RUnlock()

			// Get recent logs ordered by ID ascending to get new logs properly
			logs, err := h.events.LogsSince(ctx, lastLogID, 100, currentFilterSource)
			if err != nil {
				h.logger.Error("failed to get logs", "error", err)
				continue
			}

			if len(logs) > 0 {
				h.logger.Debug("fetched new logs", "count", len(logs), "last_id", lastLogID)
			}

			// Send new logs
			sentCount := 0
			for _, log := range logs {
				// Apply level filter if set
				if len(currentFilterLevels) > 0 {
					matched := false
					for _, level := range currentFilterLevels {
						if log.Level == level {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
				}

				// Try to send log, but don't block if channel is full
				select {
				case logChan <- log:
					sentCount++
					if log.ID > lastLogID {
						lastLogID = log.ID
					}
				case <-ctx.Done():
					h.logger.Info("context cancelled while sending logs")
					return
				default:
					// Channel is full, skip this log but update lastLogID to prevent reprocessing
					h.logger.Warn("log channel full, dropping log", "log_id", log.ID)
					if log.ID > lastLogID {
						lastLogID = log.ID
					}
				}
			}

			if sentCount > 0 {
				h.logger.Debug("sent logs to channel", "count", sentCount, "new_last_id", lastLogID)
			}

		case <-ctx.Done():
			h.logger.Info("logs websocket context cancelled")
			return
		}
	}
}
