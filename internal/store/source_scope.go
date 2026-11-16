package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"regexp"
	"slices"
	"sort"

	"alert-system/internal/domain"
)

var ErrSourceScopeChanged = errors.New("来源范围已有数据或正在变化，请创建新的 Source")
var ErrSourceScopeUnverifiable = errors.New("来源事件范围或稳定规则身份无法验证")
var publicRuleIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

const StableRuleIDLabel = "sre.rule.id"

func (c AlertmanagerSourceConfig) ValidateScope() error {
	if len(c.LabelScope) > 16 || len(c.RuleIDAllowlist) > 128 {
		return fmt.Errorf("来源范围超过上限")
	}
	seen := map[string]bool{}
	for _, m := range c.LabelScope {
		if m.Name == "" || m.Op != domain.MatchEqual || m.Value == "" || seen[m.Name] {
			return fmt.Errorf("label_scope 必须是唯一非空标签的精确等值条件")
		}
		seen[m.Name] = true
	}
	seen = map[string]bool{}
	for _, id := range c.RuleIDAllowlist {
		if !publicRuleIDPattern.MatchString(id) || seen[id] {
			return fmt.Errorf("rule_id_allowlist 包含非法或重复规则引用")
		}
		seen[id] = true
	}
	return nil
}
func (c AlertmanagerSourceConfig) ScopeKey() string {
	labels := append(domain.MatcherSet{}, c.LabelScope...)
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	ids := append([]string{}, c.RuleIDAllowlist...)
	sort.Strings(ids)
	b, _ := json.Marshal(struct {
		Labels domain.MatcherSet
		Rules  []string
	}{labels, ids})
	return string(b)
}

// FilterEvents 是 webhook 与完整快照共用的范围契约。未知合成事件不能把过滤后的
// 空快照变成恢复证据；缺少归属标签/稳定ID直接报错，由对账保留旧水位。
func (c AlertmanagerSourceConfig) FilterEvents(events []domain.AlertEvent) ([]domain.AlertEvent, error) {
	if err := c.ValidateScope(); err != nil {
		return nil, err
	}
	if len(c.LabelScope) == 0 && len(c.RuleIDAllowlist) == 0 {
		return events, nil
	}
	result := make([]domain.AlertEvent, 0, len(events))
	for _, e := range events {
		synthetic := e.RuleID == "DatasourceNoData" || e.RuleID == "DatasourceError"
		matches := true
		explicitOther := false
		for _, m := range c.LabelScope {
			v := e.Labels[m.Name]
			if v != m.Value {
				matches = false
				if v != "" {
					explicitOther = true
				}
			}
		}
		id := e.Labels[StableRuleIDLabel]
		if !matches {
			if slices.Contains(c.RuleIDAllowlist, id) {
				return nil, ErrSourceScopeUnverifiable
			}
			if synthetic && !explicitOther {
				return nil, ErrSourceScopeUnverifiable
			}
			continue
		}
		if len(c.RuleIDAllowlist) > 0 {
			id := e.Labels[StableRuleIDLabel]
			if id == "" {
				return nil, ErrSourceScopeUnverifiable
			}
			if !slices.Contains(c.RuleIDAllowlist, id) {
				continue
			}
		}
		result = append(result, e)
	}
	return result, nil
}

// ValidateExistingScope 对完整原始输入核对已登记 occurrence 的范围；必须在过滤前
// 执行，否则丢标签/错标签的老对象被当作“快照未出现”可能触发错误恢复。
func ValidateExistingScope(ctx context.Context, tx pgx.Tx, sourceID string, cfg AlertmanagerSourceConfig, events []domain.AlertEvent) error {
	if len(cfg.LabelScope) == 0 && len(cfg.RuleIDAllowlist) == 0 {
		return nil
	}
	for _, event := range events {
		var previous map[string]string
		err := tx.QueryRow(ctx, `SELECT labels FROM alerts WHERE source_integration_id=$1 AND upstream_fingerprint=$2 ORDER BY starts_at DESC LIMIT 1`, sourceID, event.UpstreamFingerprint).Scan(&previous)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		accepted, err := cfg.FilterEvents([]domain.AlertEvent{event})
		if err != nil {
			return err
		}
		if len(accepted) == 0 || previous[StableRuleIDLabel] != event.Labels[StableRuleIDLabel] {
			return ErrSourceScopeUnverifiable
		}
	}
	return nil
}
