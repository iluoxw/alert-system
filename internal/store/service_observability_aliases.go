package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// MaxAliasBatchIdentities 是单次批量 alias 查询的身份上界。
const MaxAliasBatchIdentities = 500

var (
	// ErrAliasBatchTooLarge 表示批量 alias 查询超过显式上界。
	ErrAliasBatchTooLarge = errors.New("store: alias 批量查询超过上界")
)

// AliasIdentityKey 是 (namespace, value) 查询键。Value 必须是 trim 后的字面量。
type AliasIdentityKey struct {
	Namespace string
	Value     string
}

// AliasLookupMatch 是唯一命中时的 canonical entity 投影。
type AliasLookupMatch struct {
	EntityID    string
	EntityKey   string
	Name        string
	Layer       domain.EntityLayer
	Kind        domain.ResourceNodeKind
	Environment string
}

// AliasLookupResult 是一条 identity 的批量查询结果。
//
// MatchCount 只统计 enabled claim；disabled 行不计入。0/1/2+ 分别表示 not-found/
// resolved/conflict；Match 仅在 MatchCount=1 时有值。
type AliasLookupResult struct {
	Namespace  string
	Value      string
	MatchCount int
	Match      *AliasLookupMatch
}

const aliasBatchLookupSQL = `
	WITH requested AS (
		SELECT ns, val, ord
		FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS t(ns, val, ord)
	),
	match_counts AS (
		SELECT r.ns, r.val, r.ord,
			COUNT(a.entity_id) AS match_count
		FROM requested r
		LEFT JOIN entity_aliases a
			ON a.namespace = r.ns AND a.alias_value = r.val AND a.enabled
		GROUP BY r.ns, r.val, r.ord
	),
	unique_entity AS (
		SELECT mc.ns, mc.val, mc.ord, mc.match_count, a.entity_id
		FROM match_counts mc
		LEFT JOIN LATERAL (
			SELECT entity_id
			FROM entity_aliases
			WHERE namespace = mc.ns AND alias_value = mc.val AND enabled
			LIMIT 1
		) a ON mc.match_count = 1
	)
	SELECT ue.ns, ue.val, ue.match_count,
		n.id::text, n.entity_key, n.name, n.layer, n.kind, n.environment
	FROM unique_entity ue
	LEFT JOIN resource_nodes n ON n.id = ue.entity_id
	ORDER BY ue.ord
`

// LookupAliasesBatch 在一次静态 SQL 内批量解析 external identity。
//
// 调用方必须先 dedupe；空输入不查库。disabled claim 不计入 MatchCount。
func (r *ServiceObservabilityRepo) LookupAliasesBatch(
	ctx context.Context, tx pgx.Tx, identities []AliasIdentityKey,
) ([]AliasLookupResult, error) {
	if len(identities) == 0 {
		return []AliasLookupResult{}, nil
	}
	if len(identities) > MaxAliasBatchIdentities {
		return nil, ErrAliasBatchTooLarge
	}

	namespaces := make([]string, len(identities))
	values := make([]string, len(identities))
	for i, identity := range identities {
		namespaces[i] = identity.Namespace
		values[i] = identity.Value
	}

	rows, err := tx.Query(ctx, aliasBatchLookupSQL, namespaces, values)
	if err != nil {
		return nil, fmt.Errorf("store: 批量查询 alias 失败: %w", err)
	}
	defer rows.Close()

	results := make([]AliasLookupResult, 0, len(identities))
	for rows.Next() {
		var result AliasLookupResult
		var entityID, entityKey, name, environment *string
		var layer domain.EntityLayer
		var kind domain.ResourceNodeKind
		if scanErr := rows.Scan(&result.Namespace, &result.Value, &result.MatchCount,
			&entityID, &entityKey, &name, &layer, &kind, &environment); scanErr != nil {
			return nil, fmt.Errorf("store: 扫描 alias 批量结果失败: %w", scanErr)
		}
		if result.MatchCount == 1 && entityID != nil {
			result.Match = &AliasLookupMatch{
				EntityID:    *entityID,
				EntityKey:   derefString(entityKey),
				Name:        derefString(name),
				Layer:       layer,
				Kind:        kind,
				Environment: derefString(environment),
			}
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 alias 批量结果失败: %w", err)
	}
	return results, nil
}

// DedupeAliasIdentities 去重并按 namespace、value 稳定排序。
func DedupeAliasIdentities(identities []AliasIdentityKey) []AliasIdentityKey {
	seen := make(map[string]struct{}, len(identities))
	out := make([]AliasIdentityKey, 0, len(identities))
	for _, identity := range identities {
		key := identity.Namespace + "\x1e" + identity.Value
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, identity)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Value < out[j].Value
	})
	return out
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
