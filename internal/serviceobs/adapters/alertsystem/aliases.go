package alertsystem

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/store"
)

const (
	maxAliasNamespaceLength = 64
	maxAliasValueLength     = 256
	maxAliasIdentities      = store.MaxAliasBatchIdentities
)

// AliasBatchReader 是批量 alias 查询在只读事务内的面。
type AliasBatchReader interface {
	LookupAliasesBatch(ctx context.Context, identities []store.AliasIdentityKey) ([]store.AliasLookupResult, error)
}

// AliasReadRunner 与 ReadRunner 同构，但 Reader 面暴露批量 alias 查询。
//
// Task 8 会把 LookupAliasesBatch 收敛进共享 Reader；Lane T 先独立暴露以免改动
// registration.go 的并行冲突域。
type AliasReadRunner interface {
	WithRead(ctx context.Context, fn func(AliasBatchReader) error) error
}

// AliasLimits 是 alias adapter 的有界参数。
type AliasLimits struct {
	MaxIdentities int
}

func (l AliasLimits) withDefaults() AliasLimits {
	if l.MaxIdentities <= 0 || l.MaxIdentities > maxAliasIdentities {
		l.MaxIdentities = maxAliasIdentities
	}
	return l
}

// AliasAdapter 实现 serviceobs.AliasResolver。
type AliasAdapter struct {
	runner            AliasReadRunner
	limits            AliasLimits
	allowedNamespaces map[string]struct{}
	clock             Clock
	sourceKey         string
}

// NewAliasAdapter 构造 alias adapter。allowedNamespaces 来自 compiled topology policy。
func NewAliasAdapter(
	runner AliasReadRunner, limits AliasLimits, allowedNamespaces []string, clock Clock,
) *AliasAdapter {
	allow := make(map[string]struct{}, len(allowedNamespaces))
	for _, namespace := range allowedNamespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" {
			continue
		}
		allow[namespace] = struct{}{}
	}
	return &AliasAdapter{
		runner:            runner,
		limits:            limits.withDefaults(),
		allowedNamespaces: allow,
		clock:             clock,
		sourceKey:         serviceobs.SourceKeyTopology,
	}
}

var _ serviceobs.AliasResolver = (*AliasAdapter)(nil)

// ResolveAliases 批量解析 external identity，在单个只读事务内完成。
func (a *AliasAdapter) ResolveAliases(
	ctx context.Context, identities []serviceobs.ExternalIdentity,
) (serviceobs.AliasResolution, error) {
	filtered, err := a.normalizeIdentities(identities)
	if err != nil {
		return serviceobs.AliasResolution{}, err
	}
	if len(filtered) == 0 {
		return serviceobs.AliasResolution{
			Entries: []serviceobs.AliasEntry{},
			Issues:  []serviceobs.ReconciliationIssue{},
			ReadAt:  a.clock.now(),
		}, nil
	}

	keys := store.DedupeAliasIdentities(filtered)
	if len(keys) > a.limits.MaxIdentities {
		return serviceobs.AliasResolution{}, fmt.Errorf("%w: alias identity 超过上界", serviceobs.ErrScopeTooBroad)
	}

	resolution := serviceobs.AliasResolution{
		Entries: make([]serviceobs.AliasEntry, 0, len(keys)),
		Issues:  []serviceobs.ReconciliationIssue{},
	}
	notFound := map[string]int{}
	conflicts := map[string]int{}

	err = a.runner.WithRead(ctx, func(reader AliasBatchReader) error {
		rows, lookupErr := reader.LookupAliasesBatch(ctx, keys)
		if lookupErr != nil {
			return lookupErr
		}
		for _, row := range rows {
			entry := serviceobs.AliasEntry{
				Identity: serviceobs.ExternalIdentity{Namespace: row.Namespace, Value: row.Value},
			}
			switch row.MatchCount {
			case 0:
				entry.State = serviceobs.AliasNotFound
				notFound[row.Namespace]++
			case 1:
				entry.State = serviceobs.AliasResolved
				entry.EntityID = row.Match.EntityID
				entry.Resolved = &serviceobs.AliasResolvedEntry{
					Identity:    entry.Identity,
					EntityID:    row.Match.EntityID,
					EntityKey:   row.Match.EntityKey,
					Name:        row.Match.Name,
					Layer:       model.EntityLayer(row.Match.Layer),
					Kind:        string(row.Match.Kind),
					Environment: row.Match.Environment,
				}
			default:
				entry.State = serviceobs.AliasConflict
				conflicts[row.Namespace]++
			}
			resolution.Entries = append(resolution.Entries, entry)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrAliasBatchTooLarge) {
			return serviceobs.AliasResolution{}, fmt.Errorf("%w: alias identity 超过上界", serviceobs.ErrScopeTooBroad)
		}
		return serviceobs.AliasResolution{}, fmt.Errorf("alertsystem: 批量解析 alias 失败: %w", err)
	}

	resolution.Issues = appendIssueCounts(resolution.Issues, a.sourceKey, serviceobs.ReasonAliasNotFound, notFound)
	resolution.Issues = appendIssueCounts(resolution.Issues, a.sourceKey, serviceobs.ReasonAliasConflict, conflicts)
	serviceobs.SortIssues(resolution.Issues)
	resolution.ReadAt = a.clock.now()
	return resolution, nil
}

func (a *AliasAdapter) normalizeIdentities(
	identities []serviceobs.ExternalIdentity,
) ([]store.AliasIdentityKey, error) {
	out := make([]store.AliasIdentityKey, 0, len(identities))
	for _, identity := range identities {
		namespace := strings.TrimSpace(identity.Namespace)
		value := strings.TrimSpace(identity.Value)
		if namespace == "" || value == "" {
			continue
		}
		if len(a.allowedNamespaces) > 0 {
			if _, ok := a.allowedNamespaces[namespace]; !ok {
				continue
			}
		}
		if len(namespace) > maxAliasNamespaceLength || len(value) > maxAliasValueLength {
			return nil, fmt.Errorf("%w: alias identity 字段过长", serviceobs.ErrScopeTooBroad)
		}
		out = append(out, store.AliasIdentityKey{Namespace: namespace, Value: value})
	}
	return out, nil
}

func appendIssueCounts(
	issues []serviceobs.ReconciliationIssue,
	sourceKey, reason string,
	counts map[string]int,
) []serviceobs.ReconciliationIssue {
	namespaces := make([]string, 0, len(counts))
	for namespace := range counts {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	for _, namespace := range namespaces {
		count := counts[namespace]
		if count <= 0 {
			continue
		}
		issues = append(issues, serviceobs.ReconciliationIssue{
			ReasonCode: reason,
			Namespace:  namespace,
			Count:      count,
			SourceKey:  sourceKey,
		})
	}
	return issues
}

type aliasStoreRunner struct {
	db   *store.Store
	repo *store.ServiceObservabilityRepo
}

type aliasTxReader struct {
	repo *store.ServiceObservabilityRepo
	tx   pgx.Tx
}

func (t *aliasTxReader) LookupAliasesBatch(
	ctx context.Context, identities []store.AliasIdentityKey,
) ([]store.AliasLookupResult, error) {
	return t.repo.LookupAliasesBatch(ctx, t.tx, identities)
}

// NewAliasStoreRunner 返回基于 internal/store 的真实 AliasReadRunner。
func NewAliasStoreRunner(db *store.Store) AliasReadRunner {
	return &aliasStoreRunner{db: db, repo: store.NewServiceObservabilityRepo()}
}

func (r *aliasStoreRunner) WithRead(ctx context.Context, fn func(AliasBatchReader) error) error {
	return r.db.WithReadTx(ctx, func(tx pgx.Tx) error {
		return fn(&aliasTxReader{repo: r.repo, tx: tx})
	})
}
