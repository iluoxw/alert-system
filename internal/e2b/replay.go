// Package e2b 提供脱敏、可重复的 E2B 故障回放契约，不连接真实 E2B 环境。
package e2b

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"alert-system/internal/domain"
)

//go:embed testdata/*.json
var fixtureFS embed.FS

type ReplayPhase string

const (
	ReplayPhaseStageA   ReplayPhase = "stage_a"
	ReplayPhaseStageB   ReplayPhase = "stage_b"
	ReplayPhaseRecovery ReplayPhase = "recovery"
)

type ReplaySignal struct {
	RuleID              string               `json:"rule_id"`
	SignalKind          domain.SignalKind    `json:"signal_kind"`
	Severity            domain.Severity      `json:"severity"`
	Status              domain.Status        `json:"status"`
	Title               string               `json:"title"`
	Labels              map[string]string    `json:"labels"`
	ResourceExternalRef string               `json:"resource_external_ref"`
	ResolvePolicy       domain.ResolvePolicy `json:"resolve_policy"`
	TTLSeconds          int                  `json:"ttl_seconds,omitempty"`
	Context             map[string]any       `json:"context,omitempty"`
	StartsAt            time.Time            `json:"starts_at"`
	EndsAt              time.Time            `json:"ends_at,omitempty"`
}

type ReplayObservation struct {
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	Threshold  float64   `json:"threshold"`
	Breached   bool      `json:"breached"`
	ObservedAt time.Time `json:"observed_at"`
}

type ReplayExpectations struct {
	CapabilityFaultStatus domain.FaultStatus `json:"capability_fault_status"`
	IncidentSuggested     bool               `json:"incident_suggested"`
	InstanceFaultCount    int                `json:"instance_fault_count"`
}

type ReplayFixture struct {
	SchemaVersion int                 `json:"schema_version"`
	ScenarioID    string              `json:"scenario_id"`
	Phase         ReplayPhase         `json:"phase"`
	ObservedAt    time.Time           `json:"observed_at"`
	Signals       []ReplaySignal      `json:"signals"`
	Observations  []ReplayObservation `json:"observations"`
	Expectations  ReplayExpectations  `json:"expectations"`
}

var ErrInvalidReplayFixture = errors.New("e2b: 非法回放 fixture")

func LoadReplayFixture(name string) (ReplayFixture, error) {
	clean := filepath.Base(name)
	if clean != name || !strings.HasSuffix(clean, ".json") {
		return ReplayFixture{}, fmt.Errorf("%w: 非法文件名", ErrInvalidReplayFixture)
	}
	b, err := fixtureFS.ReadFile("testdata/" + clean)
	if err != nil {
		return ReplayFixture{}, fmt.Errorf("e2b: 读取 fixture %q 失败: %w", clean, err)
	}
	var fixture ReplayFixture
	if err := json.Unmarshal(b, &fixture); err != nil {
		return ReplayFixture{}, fmt.Errorf("%w: %v", ErrInvalidReplayFixture, err)
	}
	if err := fixture.Validate(); err != nil {
		return ReplayFixture{}, err
	}
	return fixture, nil
}

func LoadTopologyFixture() (domain.TopologySnapshot, error) {
	b, err := fixtureFS.ReadFile("testdata/topology.json")
	if err != nil {
		return domain.TopologySnapshot{}, fmt.Errorf("e2b: 读取 topology fixture 失败: %w", err)
	}
	var raw topologyFixture
	if err := json.Unmarshal(b, &raw); err != nil {
		return domain.TopologySnapshot{}, fmt.Errorf("%w: %v", ErrInvalidReplayFixture, err)
	}
	snapshot := domain.TopologySnapshot{
		SchemaVersion: raw.SchemaVersion,
		Source:        raw.Source,
		Environment:   raw.Environment,
		ObservedAt:    raw.ObservedAt,
		Nodes:         raw.Nodes,
		Edges:         raw.Edges,
	}
	for i := range snapshot.Nodes {
		snapshot.Nodes[i].Source = snapshot.Source
		snapshot.Nodes[i].Environment = snapshot.Environment
		snapshot.Nodes[i].LastObservedAt = snapshot.ObservedAt
	}
	for i := range snapshot.Edges {
		snapshot.Edges[i].Source = snapshot.Source
		snapshot.Edges[i].ObservedAt = snapshot.ObservedAt
	}
	if err := snapshot.Validate(); err != nil {
		return domain.TopologySnapshot{}, err
	}
	return snapshot, nil
}

type topologyFixture struct {
	SchemaVersion int                     `json:"schema_version"`
	Source        string                  `json:"source"`
	Environment   string                  `json:"environment"`
	ObservedAt    time.Time               `json:"observed_at"`
	Nodes         []domain.ResourceNode   `json:"nodes"`
	Edges         []domain.DependencyEdge `json:"edges"`
}

func (f ReplayFixture) Validate() error {
	if f.SchemaVersion != 1 || f.ScenarioID == "" || f.ObservedAt.IsZero() {
		return fmt.Errorf("%w: 顶层字段不完整", ErrInvalidReplayFixture)
	}
	if f.Phase != ReplayPhaseStageA && f.Phase != ReplayPhaseStageB && f.Phase != ReplayPhaseRecovery {
		return fmt.Errorf("%w: phase=%q", ErrInvalidReplayFixture, f.Phase)
	}
	for i, signal := range f.Signals {
		if signal.RuleID == "" || signal.Title == "" || signal.ResourceExternalRef == "" || signal.StartsAt.IsZero() {
			return fmt.Errorf("%w: signals[%d] 字段不完整", ErrInvalidReplayFixture, i)
		}
		if !signal.Severity.IsValid() || !signal.Status.IsValid() || !signal.ResolvePolicy.IsValid() {
			return fmt.Errorf("%w: signals[%d] 枚举非法", ErrInvalidReplayFixture, i)
		}
		if signal.Labels["e2b.evidence_class"] == "" {
			return fmt.Errorf("%w: signals[%d] 缺少 e2b.evidence_class", ErrInvalidReplayFixture, i)
		}
	}
	return nil
}

// AlertEvents 将回放信号转换为现有告警管道唯一接受的统一信封。attempt_count 等
// 来源字段只进入 SignalContext，不参与 fingerprint 或 Fault 升级判定。
func (f ReplayFixture) AlertEvents() ([]domain.AlertEvent, error) {
	events := make([]domain.AlertEvent, 0, len(f.Signals))
	for _, signal := range f.Signals {
		contextJSON, err := json.Marshal(signal.Context)
		if err != nil {
			return nil, fmt.Errorf("e2b: 序列化 signal context 失败: %w", err)
		}
		labels, resourceRef := f.normalizedIdentity(signal)
		// 日志类 fingerprint 使用 resource_ref。把脱敏 SID 纳入资源标识，既不会暴露
		// 原始 Sandbox ID，又能避免多个不可恢复实例被错误 dedup 成一条 Alert。
		event := domain.AlertEvent{
			RuleID:        signal.RuleID,
			SignalKind:    signal.SignalKind,
			Severity:      signal.Severity,
			Status:        signal.Status,
			Title:         signal.Title,
			Labels:        labels,
			Source:        "e2b/replay",
			ResourceRef:   resourceRef,
			ResolvePolicy: signal.ResolvePolicy,
			TTLSeconds:    signal.TTLSeconds,
			SignalContext: contextJSON,
			StartsAt:      signal.StartsAt,
			EndsAt:        signal.EndsAt,
		}
		event.Fingerprint = domain.Fingerprint(event)
		events = append(events, event)
	}
	return events, nil
}

// GenericPayloads 生成可直接 POST 到现有 kind=generic ingest adapter 的载荷。
// 这让本地 E2E 走真实 HTTP→alert_events→pipeline，而不是绕过入口直接写 alerts。
func (f ReplayFixture) GenericPayloads() []map[string]any {
	payloads := make([]map[string]any, 0, len(f.Signals))
	for _, signal := range f.Signals {
		labels, resourceRef := f.normalizedIdentity(signal)
		payload := map[string]any{
			"rule_id": signal.RuleID, "signal_kind": signal.SignalKind,
			"severity": signal.Severity, "status": signal.Status, "title": signal.Title,
			"labels": labels, "source": "e2b/replay", "resource_ref": resourceRef,
			"resolve_policy": signal.ResolvePolicy, "starts_at": signal.StartsAt,
		}
		if signal.TTLSeconds > 0 {
			payload["ttl_seconds"] = signal.TTLSeconds
		}
		if !signal.EndsAt.IsZero() {
			payload["ends_at"] = signal.EndsAt
		}
		for key, value := range signal.Context {
			payload[key] = value
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func (f ReplayFixture) normalizedIdentity(signal ReplaySignal) (map[string]string, map[string]string) {
	labels := cloneStrings(signal.Labels)
	labels["scenario_id"] = f.ScenarioID
	resourceRef := map[string]string{"topology.external_ref": signal.ResourceExternalRef}
	if sidHash := labels["sandbox_id_hash"]; sidHash != "" {
		resourceRef["sandbox.id_hash"] = sidHash
	}
	return labels, resourceRef
}

func cloneStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
