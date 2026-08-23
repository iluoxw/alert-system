package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"alert-system/internal/serviceobs/provider"
)

// 本文件是 Phase 3 只读遥测 provider 的配置。
//
// 三条口径：
//
//  1. endpoint 为空是**合法**的"未连接"：启动成功、页面显式 Unknown。这让部署可以先上
//     alert-system 基础能力，再接遥测。
//  2. 显式配置但非法（scheme、userinfo、query、fragment、生产用 http、数值不合法）
//     一律让启动失败。静默兜底成默认值会让"配置写错"看起来像"配置生效了"。
//  3. 凭据只从服务端 env/file provider 读取，绝不进 YAML 示例明文、日志或 stats。
//     它们用 provider.Secret 承载，String/JSON/slog 全部输出掩码。

// ServiceObservabilityProvidersConfig 是两个 provider 的连接与限额配置。
type ServiceObservabilityProvidersConfig struct {
	Prometheus  ProviderEndpointConfig
	OpenObserve ProviderEndpointConfig
	// EvidenceGrantTTL 是 evidence deep link 的最长有效期。实际 TTL 取它与上游剩余
	// retention 的最小值。
	EvidenceGrantTTL time.Duration
	// AuditRetentionDays 是 tool invocation audit 的保留天数。
	AuditRetentionDays int
	// GrantCleanupBatch 是过期 grant 的单批清理行数上界。
	GrantCleanupBatch int
	// BackgroundRefreshMinInterval 是后台目录刷新对同一服务的最小间隔。
	// 它是写放大的闸门：每次刷新都会留审计行，没有下界就会把审计表刷爆。
	BackgroundRefreshMinInterval time.Duration
}

// ProviderEndpointConfig 是单个 provider 的连接配置。
type ProviderEndpointConfig struct {
	// Endpoint 已经过安全校验。零值表示未连接。
	Endpoint provider.Endpoint
	// Token 是服务账号凭据，只在 request header 注入。
	Token provider.Secret
	// Organization 是 OpenObserve 这类多租户 provider 的 org 标识。它不是凭据，但也不该
	// 进日志：org 名会泄露部署拓扑。
	Organization     string
	Timeout          time.Duration
	MaxResponseBytes int64
	MaxConcurrent    int
	MaxRows          int
	MaxSeries        int
}

// Connected 表示该 provider 是否配置了 endpoint。
func (p ProviderEndpointConfig) Connected() bool { return !p.Endpoint.IsZero() }

// String 只输出连接性与限额，绝不输出 endpoint 路径、org、用户名或凭据。
func (p ProviderEndpointConfig) String() string {
	return fmt.Sprintf(
		"ProviderEndpointConfig{Connected:%t, Host:%q, Token:%s, Timeout:%s, MaxResponseBytes:%d, MaxConcurrent:%d, MaxRows:%d, MaxSeries:%d}",
		p.Connected(), p.Endpoint.Host(), p.Token, p.Timeout,
		p.MaxResponseBytes, p.MaxConcurrent, p.MaxRows, p.MaxSeries)
}

// String 同样只输出可安全记录的项。
func (c ServiceObservabilityProvidersConfig) String() string {
	return fmt.Sprintf(
		"ServiceObservabilityProvidersConfig{Prometheus:%s, OpenObserve:%s, EvidenceGrantTTL:%s, AuditRetentionDays:%d, GrantCleanupBatch:%d, BackgroundRefreshMinInterval:%s}",
		c.Prometheus, c.OpenObserve, c.EvidenceGrantTTL, c.AuditRetentionDays,
		c.GrantCleanupBatch, c.BackgroundRefreshMinInterval)
}

type fileProviderEndpointConfig struct {
	Endpoint         string `yaml:"endpoint"`
	Organization     string `yaml:"organization"`
	Timeout          string `yaml:"timeout"`
	MaxResponseBytes int64  `yaml:"max_response_bytes"`
	MaxConcurrent    int    `yaml:"max_concurrent"`
	MaxRows          int    `yaml:"max_rows"`
	MaxSeries        int    `yaml:"max_series"`
}

type fileServiceObservabilityProvidersConfig struct {
	Prometheus  fileProviderEndpointConfig `yaml:"prometheus"`
	OpenObserve fileProviderEndpointConfig `yaml:"openobserve"`
	// 刻意没有 token 字段：YAML 里不给凭据留位置，避免示例文件被复制成带明文的部署配置。
	EvidenceGrantTTL             string `yaml:"evidence_grant_ttl"`
	AuditRetentionDays           int    `yaml:"audit_retention_days"`
	GrantCleanupBatch            int    `yaml:"grant_cleanup_batch"`
	BackgroundRefreshMinInterval string `yaml:"background_refresh_min_interval"`
}

const (
	defaultProviderTimeout              = 5 * time.Second
	defaultProviderMaxResponseBytes     = int64(8 << 20)
	defaultProviderMaxConcurrent        = 8
	defaultProviderMaxRows              = 200
	defaultProviderMaxSeries            = 500
	defaultEvidenceGrantTTL             = 15 * time.Minute
	defaultAuditRetentionDays           = 30
	defaultGrantCleanupBatch            = 500
	defaultBackgroundRefreshMinInterval = 30 * time.Second

	// maxEvidenceGrantTTL 是硬上界：一个长期有效的 deep link 等于一张长期通行证。
	maxEvidenceGrantTTL = 24 * time.Hour
	// maxProviderTimeout 防止把单来源超时配到吃掉整个请求预算。
	maxProviderTimeout = 30 * time.Second
)

func defaultFileProviders() fileServiceObservabilityProvidersConfig {
	endpoint := fileProviderEndpointConfig{
		Timeout:          defaultProviderTimeout.String(),
		MaxResponseBytes: defaultProviderMaxResponseBytes,
		MaxConcurrent:    defaultProviderMaxConcurrent,
		MaxRows:          defaultProviderMaxRows,
		MaxSeries:        defaultProviderMaxSeries,
	}
	return fileServiceObservabilityProvidersConfig{
		Prometheus:                   endpoint,
		OpenObserve:                  endpoint,
		EvidenceGrantTTL:             defaultEvidenceGrantTTL.String(),
		AuditRetentionDays:           defaultAuditRetentionDays,
		GrantCleanupBatch:            defaultGrantCleanupBatch,
		BackgroundRefreshMinInterval: defaultBackgroundRefreshMinInterval.String(),
	}
}

// normalizeServiceObservabilityProviders 校验并归一 provider 配置。
//
// mode 由调用方决定（生产装配一律 ModeProduction），因此没有任何请求参数能把它切成测试模式。
func normalizeServiceObservabilityProviders(
	fc fileServiceObservabilityProvidersConfig, mode provider.Mode,
) (ServiceObservabilityProvidersConfig, error) {
	prometheus, err := normalizeProviderEndpoint("prometheus", fc.Prometheus, mode)
	if err != nil {
		return ServiceObservabilityProvidersConfig{}, err
	}
	openObserve, err := normalizeProviderEndpoint("openobserve", fc.OpenObserve, mode)
	if err != nil {
		return ServiceObservabilityProvidersConfig{}, err
	}
	prometheus.Token = provider.NewSecret(strings.TrimSpace(os.Getenv("SERVICE_OBSERVABILITY_PROMETHEUS_TOKEN")))
	openObserve.Token = provider.NewSecret(strings.TrimSpace(os.Getenv("SERVICE_OBSERVABILITY_OPENOBSERVE_TOKEN")))

	grantTTL, err := time.ParseDuration(fc.EvidenceGrantTTL)
	if err != nil {
		return ServiceObservabilityProvidersConfig{}, fmt.Errorf(
			"invalid service_observability.providers.evidence_grant_ttl %q: %w", fc.EvidenceGrantTTL, err)
	}
	if grantTTL <= 0 || grantTTL > maxEvidenceGrantTTL {
		return ServiceObservabilityProvidersConfig{}, fmt.Errorf(
			"invalid service_observability.providers.evidence_grant_ttl %q: must be in (0, %s]",
			fc.EvidenceGrantTTL, maxEvidenceGrantTTL)
	}
	refreshInterval, err := time.ParseDuration(fc.BackgroundRefreshMinInterval)
	if err != nil {
		return ServiceObservabilityProvidersConfig{}, fmt.Errorf(
			"invalid service_observability.providers.background_refresh_min_interval %q: %w",
			fc.BackgroundRefreshMinInterval, err)
	}
	if refreshInterval <= 0 {
		return ServiceObservabilityProvidersConfig{}, fmt.Errorf(
			"invalid service_observability.providers.background_refresh_min_interval %q: must be positive",
			fc.BackgroundRefreshMinInterval)
	}
	if fc.AuditRetentionDays <= 0 {
		return ServiceObservabilityProvidersConfig{}, fmt.Errorf(
			"invalid service_observability.providers.audit_retention_days %d: must be positive",
			fc.AuditRetentionDays)
	}
	if fc.GrantCleanupBatch <= 0 {
		return ServiceObservabilityProvidersConfig{}, fmt.Errorf(
			"invalid service_observability.providers.grant_cleanup_batch %d: must be positive",
			fc.GrantCleanupBatch)
	}

	return ServiceObservabilityProvidersConfig{
		Prometheus:                   prometheus,
		OpenObserve:                  openObserve,
		EvidenceGrantTTL:             grantTTL,
		AuditRetentionDays:           fc.AuditRetentionDays,
		GrantCleanupBatch:            fc.GrantCleanupBatch,
		BackgroundRefreshMinInterval: refreshInterval,
	}, nil
}

func normalizeProviderEndpoint(
	name string, fc fileProviderEndpointConfig, mode provider.Mode,
) (ProviderEndpointConfig, error) {
	endpoint, err := provider.ParseEndpoint(fc.Endpoint, mode)
	if err != nil {
		// 错误信息只带配置项名，不回显 endpoint 原文（它可能含内部主机名）。
		return ProviderEndpointConfig{}, fmt.Errorf(
			"invalid service_observability.providers.%s.endpoint: %w", name, err)
	}
	timeout, err := time.ParseDuration(fc.Timeout)
	if err != nil {
		return ProviderEndpointConfig{}, fmt.Errorf(
			"invalid service_observability.providers.%s.timeout %q: %w", name, fc.Timeout, err)
	}
	if timeout <= 0 || timeout > maxProviderTimeout {
		return ProviderEndpointConfig{}, fmt.Errorf(
			"invalid service_observability.providers.%s.timeout %q: must be in (0, %s]",
			name, fc.Timeout, maxProviderTimeout)
	}
	if fc.MaxResponseBytes <= 0 {
		return ProviderEndpointConfig{}, fmt.Errorf(
			"invalid service_observability.providers.%s.max_response_bytes %d: must be positive",
			name, fc.MaxResponseBytes)
	}
	if fc.MaxConcurrent <= 0 {
		return ProviderEndpointConfig{}, fmt.Errorf(
			"invalid service_observability.providers.%s.max_concurrent %d: must be positive",
			name, fc.MaxConcurrent)
	}
	if fc.MaxRows <= 0 {
		return ProviderEndpointConfig{}, fmt.Errorf(
			"invalid service_observability.providers.%s.max_rows %d: must be positive", name, fc.MaxRows)
	}
	if fc.MaxSeries <= 0 {
		return ProviderEndpointConfig{}, fmt.Errorf(
			"invalid service_observability.providers.%s.max_series %d: must be positive", name, fc.MaxSeries)
	}
	return ProviderEndpointConfig{
		Endpoint:         endpoint,
		Organization:     strings.TrimSpace(fc.Organization),
		Timeout:          timeout,
		MaxResponseBytes: fc.MaxResponseBytes,
		MaxConcurrent:    fc.MaxConcurrent,
		MaxRows:          fc.MaxRows,
		MaxSeries:        fc.MaxSeries,
	}, nil
}

// applyServiceObservabilityProviderEnvOverrides 用环境变量覆盖 provider 配置。
//
// 数值项刻意不吞掉解析失败：写错的环境变量必须让启动失败，而不是静默退回默认值。
func applyServiceObservabilityProviderEnvOverrides(fc *fileConfig) {
	providers := &fc.ServiceObservability.Providers
	applyProviderEnv("PROMETHEUS", &providers.Prometheus)
	applyProviderEnv("OPENOBSERVE", &providers.OpenObserve)
	if v := os.Getenv("SERVICE_OBSERVABILITY_EVIDENCE_GRANT_TTL"); v != "" {
		providers.EvidenceGrantTTL = v
	}
	if v := os.Getenv("SERVICE_OBSERVABILITY_AUDIT_RETENTION_DAYS"); v != "" {
		providers.AuditRetentionDays = invalidIfNotInteger(v)
	}
	if v := os.Getenv("SERVICE_OBSERVABILITY_GRANT_CLEANUP_BATCH"); v != "" {
		providers.GrantCleanupBatch = invalidIfNotInteger(v)
	}
	if v := os.Getenv("SERVICE_OBSERVABILITY_BACKGROUND_REFRESH_MIN_INTERVAL"); v != "" {
		providers.BackgroundRefreshMinInterval = v
	}
}

func applyProviderEnv(prefix string, target *fileProviderEndpointConfig) {
	base := "SERVICE_OBSERVABILITY_" + prefix + "_"
	if v := os.Getenv(base + "ENDPOINT"); v != "" {
		target.Endpoint = v
	}
	if v := os.Getenv(base + "ORGANIZATION"); v != "" {
		target.Organization = v
	}
	if v := os.Getenv(base + "TIMEOUT"); v != "" {
		target.Timeout = v
	}
	if v := os.Getenv(base + "MAX_RESPONSE_BYTES"); v != "" {
		target.MaxResponseBytes = invalidIfNotInteger64(v)
	}
	if v := os.Getenv(base + "MAX_CONCURRENT"); v != "" {
		target.MaxConcurrent = invalidIfNotInteger(v)
	}
	if v := os.Getenv(base + "MAX_ROWS"); v != "" {
		target.MaxRows = invalidIfNotInteger(v)
	}
	if v := os.Getenv(base + "MAX_SERIES"); v != "" {
		target.MaxSeries = invalidIfNotInteger(v)
	}
}

// invalidIfNotInteger64 与 invalidIfNotInteger 同口径：非法值映射成必然被拒绝的 0。
func invalidIfNotInteger64(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
