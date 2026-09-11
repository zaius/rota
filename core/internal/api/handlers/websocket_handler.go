package handlers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/gorilla/websocket"
)

// WebSocketHandler handles WebSocket connections
type WebSocketHandler struct {
	dashboardRepo  *repository.DashboardRepository
	logger         *logger.Logger
	allowedOrigins []string
	upgrader       websocket.Upgrader
}

// NewWebSocketHandler creates a new WebSocketHandler. allowedOrigins is the
// configured CORS allowlist, consulted by the origin check on upgrade.
func NewWebSocketHandler(
	dashboardRepo *repository.DashboardRepository,
	log *logger.Logger,
	allowedOrigins []string,
) *WebSocketHandler {
	h := &WebSocketHandler{
		dashboardRepo:  dashboardRepo,
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
