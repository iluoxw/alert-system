package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
)

const (
	// ReasonOverlayBoundsReached 表示 observed hint 数量达到 overlay 上界。
	ReasonOverlayBoundsReached = "topology_bounds_reached"
)

// Scope 参与 observed-only relation 的 deterministic ID 派生。
type Scope struct {
	ServiceID      string
	ProfileKey     string
	ProfileVersion int
}

// Request 是纯 overlay reconcile 的输入。它只读已有 declared 拓扑与 alias 解析结果，
// 不写 canonical graph。
type Request struct {
	Scope      Scope
	Now        time.Time
	StaleAfter time.Duration
	MaxHints   int

	DeclaredRelations []serviceobs.TopologyRelation
	AliasResolution   serviceobs.AliasResolution
	TopologyHints     []serviceobs.ObservedTopologyHint
	InstanceHints     []serviceobs.ObservedInstanceHint

	// FrozenInstanceSets 是 component entity ID 到 frozen instance identity 集合。
	// 只用于 inventory 增删统计，不会创建或删除 logical component。
	FrozenInstanceSets map[string]map[string]struct{}
}

// ComponentInventory 是一个 logical component 的动态 instance 汇总。
type ComponentInventory struct {
	EntityID      string
	EntityKey     string
	Name          string
	FrozenCount   int
	ObservedCount int
	AddedCount    int
	RemovedCount  int
	ObservedAt    time.Time
}

// Result 是 overlay reconcile 的输出。
type Result struct {
	Relations []serviceobs.TopologyRelation
	Inventory []ComponentInventory
	Issues    []serviceobs.ReconciliationIssue
	Truncated bool
}

// ReconcileOverlay 把 observed topology/instance hints 合并进 declared 底图。
//
// 它是纯函数：不访问 store，也不写 dependency_edges。caller -> callee 方向固定为
// hint.Source -> hint.Target。
func ReconcileOverlay(req Request) Result {
	result := Result{
		Relations: cloneRelations(req.DeclaredRelations),
		Inventory: []ComponentInventory{},
		Issues:    append([]serviceobs.ReconciliationIssue{}, req.AliasResolution.Issues...),
	}

	aliasByIdentity := indexAliasEntries(req.AliasResolution.Entries)
	relationIndex := indexRelationPositions(result.Relations)

	hints := append([]serviceobs.ObservedTopologyHint{}, req.TopologyHints...)
	sortTopologyHints(hints)

	processed := 0
	for _, hint := range hints {
		if req.MaxHints > 0 && processed >= req.MaxHints {
			result.Truncated = true
			result.Issues = append(result.Issues, serviceobs.ReconciliationIssue{
				ReasonCode: ReasonOverlayBoundsReached,
				Count:      len(hints) - processed,
				SourceKey:  hintSourceKey(hint),
			})
			break
		}
		processed++

		sourceEntry, sourceOK := resolveAlias(aliasByIdentity, hint.Source)
		targetEntry, targetOK := resolveAlias(aliasByIdentity, hint.Target)
		if !sourceOK || !targetOK {
			result.Issues = appendAliasIssue(result.Issues, hint, sourceEntry, targetEntry)
			continue
		}

		key := relationKey{
			sourceID: sourceEntry.Resolved.EntityID,
			targetID: targetEntry.Resolved.EntityID,
			kind:     normalizeKind(hint.Kind),
		}
		stale := isStale(hint.ObservedAt, req.Now, req.StaleAfter)

		if pos, ok := relationIndex[key]; ok {
			mergeObservedIntoDeclared(&result.Relations[pos], hint, stale)
			continue
		}

		relationID := observedRelationID(req.Scope, key)
		relation := serviceobs.TopologyRelation{
			RelationID:        relationID,
			SourceEntityID:    key.sourceID,
			TargetEntityID:    key.targetID,
			Kind:              key.kind,
			Provenance:        model.ProvenanceObserved,
			ObservedAt:        hint.ObservedAt,
			Stale:             stale,
			MergedRelationIDs: []string{relationID},
		}
		relationIndex[key] = len(result.Relations)
		result.Relations = append(result.Relations, relation)
	}

	result.Relations = sortRelations(result.Relations)
	result.Inventory, result.Issues = reconcileInventory(req, aliasByIdentity, result.Issues)
	serviceobs.SortIssues(result.Issues)
	return result
}

type relationKey struct {
	sourceID string
	targetID string
	kind     string
}

func cloneRelations(relations []serviceobs.TopologyRelation) []serviceobs.TopologyRelation {
	out := make([]serviceobs.TopologyRelation, len(relations))
	copy(out, relations)
	for i := range out {
		if out[i].MergedRelationIDs == nil {
			out[i].MergedRelationIDs = []string{}
		}
	}
	return out
}

func indexRelationPositions(relations []serviceobs.TopologyRelation) map[relationKey]int {
	index := make(map[relationKey]int, len(relations))
	for i, relation := range relations {
		key := relationKey{
			sourceID: relation.SourceEntityID,
			targetID: relation.TargetEntityID,
			kind:     normalizeKind(relation.Kind),
		}
		index[key] = i
	}
	return index
}

func indexAliasEntries(entries []serviceobs.AliasEntry) map[string]serviceobs.AliasEntry {
	index := make(map[string]serviceobs.AliasEntry, len(entries))
	for _, entry := range entries {
		index[identityKey(entry.Identity)] = entry
	}
	return index
}

func identityKey(identity serviceobs.ExternalIdentity) string {
	return strings.TrimSpace(identity.Namespace) + "\x1e" + strings.TrimSpace(identity.Value)
}

func resolveAlias(
	index map[string]serviceobs.AliasEntry, identity serviceobs.ExternalIdentity,
) (serviceobs.AliasEntry, bool) {
	entry, ok := index[identityKey(identity)]
	if !ok {
		return serviceobs.AliasEntry{
			Identity: identity,
			State:    serviceobs.AliasNotFound,
		}, false
	}
	if entry.State != serviceobs.AliasResolved || entry.Resolved == nil {
		return entry, false
	}
	return entry, true
}

func appendAliasIssue(
	issues []serviceobs.ReconciliationIssue,
	hint serviceobs.ObservedTopologyHint,
	sourceEntry, targetEntry serviceobs.AliasEntry,
) []serviceobs.ReconciliationIssue {
	sourceKey := hintSourceKey(hint)
	if sourceEntry.State == serviceobs.AliasNotFound {
		issues = bumpIssue(issues, serviceobs.ReasonAliasNotFound, hint.Source.Namespace, sourceKey)
	}
	if sourceEntry.State == serviceobs.AliasConflict {
		issues = bumpIssue(issues, serviceobs.ReasonAliasConflict, hint.Source.Namespace, sourceKey)
	}
	if targetEntry.State == serviceobs.AliasNotFound {
		issues = bumpIssue(issues, serviceobs.ReasonAliasNotFound, hint.Target.Namespace, sourceKey)
	}
	if targetEntry.State == serviceobs.AliasConflict {
		issues = bumpIssue(issues, serviceobs.ReasonAliasConflict, hint.Target.Namespace, sourceKey)
	}
	return issues
}

func bumpIssue(
	issues []serviceobs.ReconciliationIssue, reason, namespace, sourceKey string,
) []serviceobs.ReconciliationIssue {
	for i := range issues {
		if issues[i].ReasonCode == reason && issues[i].Namespace == namespace && issues[i].SourceKey == sourceKey {
			issues[i].Count++
			return issues
		}
	}
	return append(issues, serviceobs.ReconciliationIssue{
		ReasonCode: reason,
		Namespace:  namespace,
		Count:      1,
		SourceKey:  sourceKey,
	})
}

func mergeObservedIntoDeclared(
	relation *serviceobs.TopologyRelation, hint serviceobs.ObservedTopologyHint, stale bool,
) {
	switch relation.Provenance {
	case model.ProvenanceDeclared:
		relation.Provenance = model.ProvenanceDeclaredAndObserved
	case model.ProvenanceObserved:
		// observed-only 边再次看到同方向 hint，只刷新时间与 stale。
	case model.ProvenanceDeclaredAndObserved:
	default:
		relation.Provenance = model.ProvenanceObserved
	}
	if hint.ObservedAt.After(relation.ObservedAt) {
		relation.ObservedAt = hint.ObservedAt
	}
	relation.Stale = stale
}

func observedRelationID(scope Scope, key relationKey) string {
	payload := strings.Join([]string{
		scope.ServiceID,
		scope.ProfileKey,
		fmt.Sprintf("%d", scope.ProfileVersion),
		key.sourceID,
		key.targetID,
		key.kind,
	}, "\x1e")
	sum := sha256.Sum256([]byte(payload))
	return "obs_" + hex.EncodeToString(sum[:16])
}

func isStale(observedAt, now time.Time, staleAfter time.Duration) bool {
	if observedAt.IsZero() || staleAfter <= 0 || now.IsZero() {
		return false
	}
	return observedAt.Add(staleAfter).Before(now)
}

func normalizeKind(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "depends_on"
	}
	return kind
}

func hintSourceKey(hint serviceobs.ObservedTopologyHint) string {
	if hint.SourceKey != "" {
		return hint.SourceKey
	}
	return serviceobs.SourceKeyTopology
}

func sortTopologyHints(hints []serviceobs.ObservedTopologyHint) {
	sort.SliceStable(hints, func(i, j int) bool {
		left := identityKey(hints[i].Source) + "\x1f" + identityKey(hints[i].Target) + "\x1f" + hints[i].Kind
		right := identityKey(hints[j].Source) + "\x1f" + identityKey(hints[j].Target) + "\x1f" + hints[j].Kind
		if left != right {
			return left < right
		}
		return hints[i].ObservedAt.Before(hints[j].ObservedAt)
	})
}

func sortRelations(relations []serviceobs.TopologyRelation) []serviceobs.TopologyRelation {
	sort.SliceStable(relations, func(i, j int) bool {
		if relations[i].SourceEntityID != relations[j].SourceEntityID {
			return relations[i].SourceEntityID < relations[j].SourceEntityID
		}
		if relations[i].TargetEntityID != relations[j].TargetEntityID {
			return relations[i].TargetEntityID < relations[j].TargetEntityID
		}
		if relations[i].Kind != relations[j].Kind {
			return relations[i].Kind < relations[j].Kind
		}
		return relations[i].RelationID < relations[j].RelationID
	})
	return relations
}

func reconcileInventory(
	req Request, aliasByIdentity map[string]serviceobs.AliasEntry, issues []serviceobs.ReconciliationIssue,
) ([]ComponentInventory, []serviceobs.ReconciliationIssue) {
	type componentState struct {
		entry      serviceobs.AliasResolvedEntry
		instances  map[string]struct{}
		observedAt time.Time
	}
	components := map[string]*componentState{}

	for _, hint := range req.InstanceHints {
		componentEntry, componentOK := resolveAlias(aliasByIdentity, hint.Component)
		instanceEntry, instanceOK := resolveAlias(aliasByIdentity, hint.Instance)
		sourceKey := hintSourceKeyFromInstance(hint)
		if !componentOK {
			if componentEntry.State == serviceobs.AliasNotFound {
				issues = bumpIssue(issues, serviceobs.ReasonAliasNotFound, hint.Component.Namespace, sourceKey)
			}
			if componentEntry.State == serviceobs.AliasConflict {
				issues = bumpIssue(issues, serviceobs.ReasonAliasConflict, hint.Component.Namespace, sourceKey)
			}
			continue
		}
		if !instanceOK {
			if instanceEntry.State == serviceobs.AliasNotFound {
				issues = bumpIssue(issues, serviceobs.ReasonAliasNotFound, hint.Instance.Namespace, sourceKey)
			}
			if instanceEntry.State == serviceobs.AliasConflict {
				issues = bumpIssue(issues, serviceobs.ReasonAliasConflict, hint.Instance.Namespace, sourceKey)
			}
			continue
		}
		entityID := componentEntry.Resolved.EntityID
		state, ok := components[entityID]
		if !ok {
			state = &componentState{
				entry:     *componentEntry.Resolved,
				instances: map[string]struct{}{},
			}
			components[entityID] = state
		}
		state.instances[identityKey(instanceEntry.Identity)] = struct{}{}
		if hint.ObservedAt.After(state.observedAt) {
			state.observedAt = hint.ObservedAt
		}
	}

	inventory := make([]ComponentInventory, 0, len(components))
	entityIDs := make([]string, 0, len(components))
	for entityID := range components {
		entityIDs = append(entityIDs, entityID)
	}
	sort.Strings(entityIDs)

	for _, entityID := range entityIDs {
		state := components[entityID]
		frozen := req.FrozenInstanceSets[entityID]
		added, removed := diffInstanceSets(frozen, state.instances)
		inventory = append(inventory, ComponentInventory{
			EntityID:      entityID,
			EntityKey:     state.entry.EntityKey,
			Name:          state.entry.Name,
			FrozenCount:   len(frozen),
			ObservedCount: len(state.instances),
			AddedCount:    added,
			RemovedCount:  removed,
			ObservedAt:    state.observedAt,
		})
	}
	return inventory, issues
}

func hintSourceKeyFromInstance(hint serviceobs.ObservedInstanceHint) string {
	if hint.SourceKey != "" {
		return hint.SourceKey
	}
	return serviceobs.SourceKeyTopology
}

func diffInstanceSets(frozen, observed map[string]struct{}) (added, removed int) {
	if frozen == nil {
		frozen = map[string]struct{}{}
	}
	for key := range observed {
		if _, ok := frozen[key]; !ok {
			added++
		}
	}
	for key := range frozen {
		if _, ok := observed[key]; !ok {
			removed++
		}
	}
	return added, removed
}
