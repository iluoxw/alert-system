// Package alertsource 实现 Alertmanager 逻辑数据源的只读 API 对账。
package alertsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/ingest"
	"alert-system/internal/store"
)

const maxSnapshotBodyBytes = 16 << 20

type Client struct {
	httpClient *http.Client
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{httpClient: httpClient}
}

func ValidateConfig(cfg store.AlertmanagerSourceConfig) error {
	cfg = cfg.WithDefaults()
	if cfg.APIURL == "" {
		return fmt.Errorf("api_url 不能为空")
	}
	u, err := url.Parse(cfg.APIURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("api_url 必须是 http/https URL")
	}
	if u.User != nil {
		return fmt.Errorf("api_url 不允许内嵌用户名或密码")
	}
	if cfg.ReconcileIntervalSeconds < 5 || cfg.ReconcileIntervalSeconds > 3600 {
		return fmt.Errorf("reconcile_interval_seconds 必须在 5~3600 之间")
	}
	if cfg.StaleAfterSeconds < cfg.ReconcileIntervalSeconds || cfg.StaleAfterSeconds > 86400 {
		return fmt.Errorf("stale_after_seconds 必须不小于对账周期且不超过 86400")
	}
	if cfg.MissingConfirmations < 2 || cfg.MissingConfirmations > 20 {
		return fmt.Errorf("missing_confirmations 必须在 2~20 之间")
	}
	return nil
}

func (c *Client) Fetch(ctx context.Context, source store.AlertSource) ([]domain.AlertEvent, error) {
	if err := ValidateConfig(source.Config); err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(source.Config.APIURL, "/") + "/api/v2/alerts"
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("alertsource: 构造 alerts API URL 失败: %w", err)
	}
	q := u.Query()
	q.Set("active", "true")
	q.Set("silenced", "true")
	q.Set("inhibited", "true")
	q.Set("unprocessed", "true")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("alertsource: 构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if envName := source.Config.BearerTokenEnv; envName != "" {
		token := os.Getenv(envName)
		if token == "" {
			return nil, fmt.Errorf("alertsource: 凭据环境变量 %s 未设置", envName)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alertsource: 请求 Alertmanager 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("alertsource: 读取 Alertmanager 响应失败: %w", err)
	}
	if len(body) > maxSnapshotBodyBytes {
		return nil, fmt.Errorf("alertsource: Alertmanager 响应超过 %d 字节", maxSnapshotBodyBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return nil, fmt.Errorf("alertsource: Alertmanager 返回 HTTP %d: %s", resp.StatusCode, snippet)
	}
	var upstream []ingest.AlertmanagerAPIAlert
	if err := json.Unmarshal(body, &upstream); err != nil {
		return nil, fmt.Errorf("alertsource: 解析 Alertmanager alerts 响应失败: %w", err)
	}
	defaultSeverity := domain.Severity(source.DefaultSeverity)
	if !defaultSeverity.IsValid() {
		defaultSeverity = domain.SeverityWarning
	}
	events := make([]domain.AlertEvent, 0, len(upstream))
	for i, alert := range upstream {
		event, err := ingest.AlertmanagerAPIEvent(alert, defaultSeverity)
		if err != nil {
			return nil, fmt.Errorf("alertsource: 第 %d 条 Alert 无效: %w", i, err)
		}
		events = append(events, event)
	}
	return events, nil
}
