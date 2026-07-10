package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// AlertWatcher monitors pool health and fires webhook alerts when active proxies
// drop below the configured threshold.
type AlertWatcher struct {
	poolRepo *repository.PoolRepository
	log      *logger.Logger
	client   *http.Client
	interval time.Duration
}

// NewAlertWatcher creates a new AlertWatcher with a 2-minute check interval.
func NewAlertWatcher(poolRepo *repository.PoolRepository, log *logger.Logger) *AlertWatcher {
	return &AlertWatcher{
		poolRepo: poolRepo,
		log:      log,
		client:   &http.Client{Timeout: 10 * time.Second},
		interval: 2 * time.Minute,
	}
}

// Name identifies the service for the lifecycle manager.
func (w *AlertWatcher) Name() string { return "alert-watcher" }

// Run checks pool alert rules on the configured interval until ctx is cancelled.
func (w *AlertWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.check(ctx)
		}
	}
}

// check loads all enabled rules and fires those whose thresholds are exceeded
// and whose cooldown has elapsed.
func (w *AlertWatcher) check(ctx context.Context) {
	rules, err := w.poolRepo.GetAllAlertRules(ctx)
	if err != nil {
		w.log.Warn("alert watcher: failed to load rules", "error", err)
		return
	}
	if len(rules) == 0 {
		return
	}

	for _, rule := range rules {
		pool, err := w.poolRepo.GetByID(ctx, rule.PoolID)
		if err != nil || pool == nil {
			continue
		}

		if pool.ActiveProxies >= rule.MinActiveProxies {
			continue // threshold OK
		}

		// Check cooldown
		if rule.LastFiredAt != nil {
			cooldown := time.Duration(rule.CooldownMinutes) * time.Minute
			if time.Since(*rule.LastFiredAt) < cooldown {
				continue // still in cooldown
			}
		}

		w.log.Warn("pool alert threshold triggered",
			"pool_id", pool.ID,
			"pool_name", pool.Name,
			"active", pool.ActiveProxies,
			"threshold", rule.MinActiveProxies,
		)

		if err := w.fire(ctx, rule, *pool); err != nil {
			w.log.Error("failed to fire pool alert webhook",
				"rule_id", rule.ID,
				"url", redactWebhookURL(rule.WebhookURL),
				"error", err,
			)
		} else {
			// Record fire time
			if err := w.poolRepo.UpdateAlertRuleFiredAt(ctx, rule.ID); err != nil {
				w.log.Warn("failed to update alert rule fired_at", "rule_id", rule.ID, "error", err)
			}
		}
	}
}

// redactWebhookURL returns a form of the webhook URL that is safe to log.
// Log entries are persisted to the database by the logger's hook, so a webhook
// secret written here outlives the process. Telegram embeds its bot token in
// the /bot<token>/ path segment; other providers tend to put secrets in the
// query string or in userinfo. All three are stripped.
func redactWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted]"
	}
	path := u.Path
	if strings.HasPrefix(path, "/bot") {
		rest := strings.TrimPrefix(path, "/bot")
		if i := strings.IndexByte(rest, '/'); i != -1 {
			path = "/bot<redacted>" + rest[i:]
		} else {
			path = "/bot<redacted>"
		}
	}
	return u.Scheme + "://" + u.Host + path
}

// fire sends the webhook request for a triggered alert rule.
func (w *AlertWatcher) fire(ctx context.Context, rule models.PoolAlertRule, pool models.ProxyPool) error {
	payload := models.PoolAlertPayload{
		Event:         "pool.degraded",
		PoolID:        pool.ID,
		PoolName:      pool.Name,
		ActiveProxies: pool.ActiveProxies,
		TotalProxies:  pool.TotalProxies,
		Threshold:     rule.MinActiveProxies,
		FiredAt:       time.Now().UTC(),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	method := rule.WebhookMethod
	if method == "" {
		method = "POST"
	}

	req, err := http.NewRequestWithContext(ctx, method, rule.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Rota-AlertWatcher/1.0")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}

	w.log.Info("pool alert webhook fired",
		"rule_id", rule.ID,
		"pool_id", pool.ID,
		"status", resp.StatusCode,
	)
	return nil
}
