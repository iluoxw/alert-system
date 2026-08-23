package openobserve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"alert-system/internal/serviceobs/integration"
)

// StreamManifest 是单条 stream 的 bounded discovery 摘要。
type StreamManifest struct {
	Name              string          `json:"name"`
	Type              string          `json:"type"`
	Fields            []FieldManifest `json:"fields"`
	MinObservedTime   string          `json:"min_observed_time,omitempty"`
	MaxObservedTime   string          `json:"max_observed_time,omitempty"`
	RetentionExplicit bool            `json:"retention_explicit"`
	RetentionUnknown  bool            `json:"retention_unknown"`
	RequiredFields    FieldPresence   `json:"required_fields"`
}

// FieldManifest 是 stream schema 中的字段摘要。
type FieldManifest struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// FieldPresence 记录 required focus/stage/identity 字段是否存在。
type FieldPresence struct {
	FocusField     bool            `json:"focus_field"`
	StageField     bool            `json:"stage_field"`
	StatusField    bool            `json:"status_field"`
	IdentityFields map[string]bool `json:"identity_fields"`
}

// DiscoveryManifest 是 bounded 只读 stream discovery 产物。
type DiscoveryManifest struct {
	Streams           []StreamManifest `json:"streams"`
	ContentDigest     string           `json:"content_digest"`
	CapabilityFlags   []string         `json:"capability_flags"`
	ProductionBlocked bool             `json:"production_blocked"`
	BlockedReasons    []string         `json:"blocked_reasons,omitempty"`
}

type streamListResponse struct {
	List []struct {
		Name       string `json:"name"`
		StreamType string `json:"stream_type"`
	} `json:"list"`
}

type streamSchemaResponse struct {
	Schema []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"schema"`
}

type streamSettingsResponse struct {
	Retention struct {
		Period string `json:"period"`
	} `json:"retention"`
}

type streamStatsResponse struct {
	Stats struct {
		MinTime int64 `json:"min_ts"`
		MaxTime int64 `json:"max_ts"`
	} `json:"stats"`
}

// BuildDiscoveryManifest 从 bounded 只读 endpoint 响应构造 manifest。
//
// 不保存字段值、样本、正文、endpoint、organization、用户名或凭据。
func BuildDiscoveryManifest(
	streamsBody, schemaBody, settingsBody, statsBody []byte,
	requirements integration.TemplateParams,
) (DiscoveryManifest, error) {
	var list streamListResponse
	if err := json.Unmarshal(streamsBody, &list); err != nil {
		return DiscoveryManifest{}, ErrParseResponse
	}

	manifest := DiscoveryManifest{
		CapabilityFlags: []string{"search_sql", "stream_schema", "stream_stats"},
	}
	if len(list.List) == 0 {
		manifest.ProductionBlocked = true
		manifest.BlockedReasons = append(manifest.BlockedReasons, "no_streams")
	}

	var schema streamSchemaResponse
	if err := json.Unmarshal(schemaBody, &schema); err != nil {
		return DiscoveryManifest{}, ErrParseResponse
	}
	fieldSet := map[string]string{}
	for _, field := range schema.Schema {
		fieldSet[field.Name] = field.Type
	}

	presence := FieldPresence{IdentityFields: map[string]bool{}}
	if requirements.FocusField != "" {
		_, presence.FocusField = fieldSet[requirements.FocusField]
	}
	if requirements.StageField != "" {
		_, presence.StageField = fieldSet[requirements.StageField]
	}
	if requirements.StatusField != "" {
		_, presence.StatusField = fieldSet[requirements.StatusField]
	}
	for _, mapping := range requirements.IdentityMappings {
		_, ok := fieldSet[mapping.Field]
		presence.IdentityFields[mapping.Field] = ok
	}

	retentionExplicit := false
	retentionUnknown := true
	var settings streamSettingsResponse
	if err := json.Unmarshal(settingsBody, &settings); err == nil {
		if strings.TrimSpace(settings.Retention.Period) != "" {
			retentionExplicit = true
			retentionUnknown = false
		}
	}

	minTime := ""
	maxTime := ""
	var stats streamStatsResponse
	if err := json.Unmarshal(statsBody, &stats); err == nil {
		if stats.Stats.MinTime > 0 {
			minTime = time.UnixMicro(stats.Stats.MinTime).UTC().Format(time.RFC3339)
		}
		if stats.Stats.MaxTime > 0 {
			maxTime = time.UnixMicro(stats.Stats.MaxTime).UTC().Format(time.RFC3339)
		}
	}

	fields := make([]FieldManifest, 0, len(schema.Schema))
	for _, field := range schema.Schema {
		fields = append(fields, FieldManifest{Name: field.Name, Type: field.Type})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })

	streamName := ""
	streamType := ""
	if len(list.List) > 0 {
		streamName = list.List[0].Name
		streamType = list.List[0].StreamType
	}
	manifest.Streams = []StreamManifest{{
		Name:              streamName,
		Type:              streamType,
		Fields:            fields,
		MinObservedTime:   minTime,
		MaxObservedTime:   maxTime,
		RetentionExplicit: retentionExplicit,
		RetentionUnknown:  retentionUnknown,
		RequiredFields:    presence,
	}}

	if requirements.FocusField != "" && !presence.FocusField {
		manifest.ProductionBlocked = true
		manifest.BlockedReasons = append(manifest.BlockedReasons, "missing_focus_field")
	}
	if requirements.StageField != "" && !presence.StageField {
		manifest.ProductionBlocked = true
		manifest.BlockedReasons = append(manifest.BlockedReasons, "missing_stage_field")
	}
	if len(requirements.StageMappings) > 0 && !presence.StageField {
		manifest.ProductionBlocked = true
		manifest.BlockedReasons = append(manifest.BlockedReasons, "journey_mapping_unconfirmed")
	}
	if retentionUnknown {
		manifest.ProductionBlocked = true
		manifest.BlockedReasons = append(manifest.BlockedReasons, "retention_unconfirmed")
	}

	canonical, err := json.Marshal(manifest.Streams)
	if err != nil {
		return DiscoveryManifest{}, fmt.Errorf("openobserve: manifest digest 失败")
	}
	sum := sha256.Sum256(canonical)
	manifest.ContentDigest = hex.EncodeToString(sum[:])
	sort.Strings(manifest.BlockedReasons)
	return manifest, nil
}

// DiscoveryPaths 返回 bounded 只读 discovery endpoint 路径。
func DiscoveryPaths(org, stream string) (streamsPath, schemaPath, settingsPath, statsPath string, err error) {
	orgSegment, err := encodePathSegment(org)
	if err != nil {
		return "", "", "", "", err
	}
	streamSegment, err := encodePathSegment(stream)
	if err != nil {
		return "", "", "", "", err
	}
	base := "api/" + orgSegment + "/streams"
	return base, base + "/" + streamSegment + "/schema",
		base + "/" + streamSegment + "/settings",
		base + "/" + streamSegment + "/stats", nil
}
