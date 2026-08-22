package model

// EvidenceAvailability 是 EvidenceRef 专属的可用性词表。它是唯一带 expired 的词表：
// 源系统 retention 到期后摘要仍在、原文已不可取。projector 绝不能把 expired 写进
// signal 或 source 的 availability——那两处过期只用 freshness=stale/unknown 表达。
type EvidenceAvailability string

const (
	EvidenceAvailabilityAvailable   EvidenceAvailability = "available"
	EvidenceAvailabilityExpired     EvidenceAvailability = "expired"
	EvidenceAvailabilityForbidden   EvidenceAvailability = "forbidden"
	EvidenceAvailabilityUnavailable EvidenceAvailability = "unavailable"
)

func (a EvidenceAvailability) IsValid() bool {
	switch a {
	case EvidenceAvailabilityAvailable, EvidenceAvailabilityExpired,
		EvidenceAvailabilityForbidden, EvidenceAvailabilityUnavailable:
		return true
	default:
		return false
	}
}

type EvidenceSource string

const (
	EvidenceSourceOpenObserve          EvidenceSource = "openobserve"
	EvidenceSourcePrometheusCompatible EvidenceSource = "prometheus_compatible"
	EvidenceSourceAlertSystem          EvidenceSource = "alert_system"
	EvidenceSourceAlertmanager         EvidenceSource = "alertmanager"
	EvidenceSourceTopology             EvidenceSource = "topology"
	EvidenceSourceHuman                EvidenceSource = "human"
)

func (s EvidenceSource) IsValid() bool {
	switch s {
	case EvidenceSourceOpenObserve, EvidenceSourcePrometheusCompatible, EvidenceSourceAlertSystem,
		EvidenceSourceAlertmanager, EvidenceSourceTopology, EvidenceSourceHuman:
		return true
	default:
		return false
	}
}

type EvidenceKind string

const (
	EvidenceKindMetric           EvidenceKind = "metric"
	EvidenceKindLog              EvidenceKind = "log"
	EvidenceKindTrace            EvidenceKind = "trace"
	EvidenceKindAlert            EvidenceKind = "alert"
	EvidenceKindTopologySnapshot EvidenceKind = "topology_snapshot"
	EvidenceKindFault            EvidenceKind = "fault"
	EvidenceKindImpact           EvidenceKind = "impact"
	EvidenceKindIncident         EvidenceKind = "incident"
	EvidenceKindHumanDecision    EvidenceKind = "human_decision"
)

func (k EvidenceKind) IsValid() bool {
	switch k {
	case EvidenceKindMetric, EvidenceKindLog, EvidenceKindTrace, EvidenceKindAlert,
		EvidenceKindTopologySnapshot, EvidenceKindFault, EvidenceKindImpact,
		EvidenceKindIncident, EvidenceKindHumanDecision:
		return true
	default:
		return false
	}
}

type Sensitivity string

const (
	SensitivityPublic     Sensitivity = "public"
	SensitivityInternal   Sensitivity = "internal"
	SensitivityRestricted Sensitivity = "restricted"
)

func (s Sensitivity) IsValid() bool {
	switch s {
	case SensitivityPublic, SensitivityInternal, SensitivityRestricted:
		return true
	default:
		return false
	}
}

// EvidenceRef 镜像 evidence-ref.schema.json：指向源系统证据的有限、可审计引用。
// 原始日志正文、trace attributes 与凭据留在源系统，这里只保留脱敏摘要、相关键与
// 受控 locator。
type EvidenceRef struct {
	SchemaVersion string               `json:"schema_version"`
	EvidenceID    string               `json:"evidence_id"`
	Source        EvidenceSource       `json:"source"`
	Kind          EvidenceKind         `json:"kind"`
	Locator       EvidenceLocator      `json:"locator"`
	Correlation   EvidenceCorrelation  `json:"correlation"`
	ObservedAt    Timestamp            `json:"observed_at"`
	FetchedAt     Timestamp            `json:"fetched_at"`
	ExpiresAt     Timestamp            `json:"expires_at,omitempty"`
	Freshness     Freshness            `json:"freshness"`
	Availability  EvidenceAvailability `json:"availability"`
	Summary       string               `json:"summary"`
	Integrity     *EvidenceIntegrity   `json:"integrity,omitempty"`
	Sensitivity   Sensitivity          `json:"sensitivity"`
	Redactions    []string             `json:"redactions"`
}

// EvidenceLocator 至少要有 record_id、query_template_ref 或 deep_link 之一。
// DeepLink 由 alert-system 生成，并在打开时再次鉴权，不是可直连的源系统 URL。
type EvidenceLocator struct {
	Provider         string `json:"provider"`
	Dataset          string `json:"dataset,omitempty"`
	RecordID         string `json:"record_id,omitempty"`
	QueryTemplateRef string `json:"query_template_ref,omitempty"`
	DeepLink         string `json:"deep_link,omitempty"`
}

type EvidenceCorrelation struct {
	ServiceID   string         `json:"service_id"`
	Focus       *EvidenceFocus `json:"focus,omitempty"`
	EntityIDs   []string       `json:"entity_ids,omitempty"`
	RelationIDs []string       `json:"relation_ids,omitempty"`
	AlertIDs    []string       `json:"alert_ids,omitempty"`
}

type EvidenceFocus struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type EvidenceIntegrity struct {
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
}

func (e *EvidenceRef) Normalize() {
	e.Redactions = nonNilSlice(e.Redactions)
}
