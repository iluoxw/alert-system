package agentdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/agentprotocol"
	"alert-system/internal/domain"
	"alert-system/internal/store"
)

type messageWriter interface {
	Enqueue(context.Context, pgx.Tx, Envelope, Policy) error
}

type Projector struct {
	store   *store.Store
	queue   messageWriter
	routing Routing
	log     *slog.Logger
}

func NewProjector(st *store.Store, queue messageWriter, routing Routing, logger *slog.Logger) *Projector {
	if logger == nil {
		logger = slog.Default()
	}
	return &Projector{store: st, queue: queue, routing: routing, log: logger}
}

// Record receives an authenticated source ID and the validated, normalized members of one webhook.
func (p *Projector) Record(ctx context.Context, tx pgx.Tx, sourceID string, raw json.RawMessage, events []domain.AlertEvent) error {
	return p.record(ctx, tx, sourceID, raw, events, "webhook")
}
func (p *Projector) RecordSnapshot(ctx context.Context, tx pgx.Tx, sourceID string, raw json.RawMessage, events []domain.AlertEvent) error {
	return p.record(ctx, tx, sourceID, raw, events, "snapshot")
}
func (p *Projector) record(ctx context.Context, tx pgx.Tx, sourceID string, raw json.RawMessage, events []domain.AlertEvent, origin string) error {
	if !slices.Contains(p.routing.SourceIDs, sourceID) {
		return nil
	}
	if len(events) > 0 && events[0].Source == domain.SourceKindGrafana {
		// 唯一收据与batch同事务；不依赖消息留存，ACK或retention后迟到也不再分析。
		ordered := slices.Clone(events)
		receiptKey := func(e domain.AlertEvent) string {
			return agentprotocol.OccurrenceRef(e.Source, sourceID, e.UpstreamFingerprint, e.StartsAt) + string(e.Status)
		}
		sort.Slice(ordered, func(i, j int) bool { return receiptKey(ordered[i]) < receiptKey(ordered[j]) })
		freshCount := int64(0)
		for _, e := range ordered {
			ref := agentprotocol.OccurrenceRef(e.Source, sourceID, e.UpstreamFingerprint, e.StartsAt)
			tag, err := tx.Exec(ctx, `INSERT INTO agent_grafana_occurrences(source_id,occurrence_ref,phase) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, sourceID, ref, string(e.Status))
			if err != nil {
				return err
			}
			freshCount += tag.RowsAffected()
		}
		// 有新阶段时保留整个已通过scope的上游组上下文；不能把仍firing的组
		// 因仅剩一条新resolved成员而伪装成完整恢复。
		if freshCount == 0 {
			return nil
		}
	}
	env, policies, err := projectGroup(sourceID, raw, events, p.routing)
	if err != nil {
		return err
	}
	if env.Source == domain.SourceKindGrafana {
		var payload agentprotocol.GroupPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return err
		}
		payload.Origin = origin
		env.Payload, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	env.TraceID = uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO agent_batches (dedup_key, raw_webhook, envelope, policies)
		VALUES ($1,$2,$3,$4)`, env.DedupKey, []byte(raw), env, policies)
	return err
}

type groupFields struct {
	GroupKey        string            `json:"groupKey"`
	Receiver        string            `json:"receiver"`
	Status          string            `json:"status"`
	TruncatedAlerts int               `json:"truncatedAlerts"`
	GroupLabels     map[string]string `json:"groupLabels"`
	CommonLabels    map[string]string `json:"commonLabels"`
}

// The internal copy retains the full webhook. Only these explicit fields cross the machine API.
type projectedMember = agentprotocol.Member
type projectedPayload = agentprotocol.GroupPayload

func digestFields(values ...string) string {
	// JSON array encoding includes field boundaries (group names may themselves contain separators).
	data, _ := json.Marshal(values)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func projectGroup(sourceID string, raw json.RawMessage, events []domain.AlertEvent, routing Routing) (Envelope, []Policy, error) {
	var fields groupFields
	if len(events) == 0 || json.Unmarshal(raw, &fields) != nil {
		return Envelope{}, nil, errors.New("invalid analysis group")
	}
	labels := make(map[string]string, len(fields.CommonLabels)+len(fields.GroupLabels))
	maps.Copy(labels, fields.CommonLabels)
	maps.Copy(labels, fields.GroupLabels)
	sourceKind := events[0].Source
	if sourceKind == "" {
		sourceKind = domain.SourceKindAlertmanager
	} // 兼容旧测试/历史内部调用
	if !domain.IsAuthoritativeSourceKind(sourceKind) {
		return Envelope{}, nil, errors.New("unsupported analysis source")
	}
	for _, event := range events {
		if event.Source != "" && event.Source != sourceKind {
			return Envelope{}, nil, errors.New("mixed analysis sources")
		}
	}
	if sourceKind == domain.SourceKindGrafana {
		// 范围过滤后不可继续用原始混合组的 commonLabels 来挑选角色。
		labels = make(map[string]string)
		for k, v := range events[0].Labels {
			labels[k] = v
		}
		for _, event := range events[1:] {
			for k, v := range labels {
				if event.Labels[k] != v {
					delete(labels, k)
				}
			}
		}
	}
	env := Envelope{
		SchemaVersion: 1, SourceID: sourceID, Source: sourceKind,
		EventType: "alert.resolved", Severity: string(domain.SeverityInfo), Labels: map[string]string{},
	}
	if fields.Status == "firing" && fields.TruncatedAlerts > 0 {
		// A firing member may have been omitted by Alertmanager's max_alerts.
		env.EventType = "alert.firing"
	}
	payload := projectedPayload{TruncatedAlerts: fields.TruncatedAlerts, Alerts: make([]projectedMember, 0, len(events))}
	for _, event := range events {
		if event.Status == domain.StatusFiring {
			env.EventType = "alert.firing"
		}
		if event.Severity == domain.SeverityCritical || (event.Severity == domain.SeverityWarning && env.Severity == string(domain.SeverityInfo)) {
			env.Severity = string(event.Severity)
		}
		member := projectedMember{
			Ref:    agentprotocol.OccurrenceRef(sourceKind, sourceID, event.UpstreamFingerprint, event.StartsAt),
			Status: event.Status, StartsAt: event.StartsAt, Severity: event.Severity,
		}
		if !event.EndsAt.IsZero() {
			endsAt := event.EndsAt
			member.EndsAt = &endsAt
		}
		ruleID := event.Labels["sre.rule.id"]
		if namePattern.MatchString(ruleID) && slices.Contains(routing.PublicRuleIDs, ruleID) {
			member.RuleID = ruleID
		}
		if namePattern.MatchString(event.RuleID) {
			member.Alertname = event.RuleID
		}
		payload.Alerts = append(payload.Alerts, member)
	}
	policies := selectPolicies(routing, env, labels)
	for _, key := range []string{"alertname", "severity"} {
		if value := labels[key]; namePattern.MatchString(value) {
			env.Labels[key] = value
		}
	}
	groupIdentity := []string{fields.GroupKey}
	if fields.GroupKey == "" {
		refs := make([]string, 0, len(events))
		for _, m := range payload.Alerts {
			refs = append(refs, m.Ref)
		}
		sort.Strings(refs)
		groupIdentity = append(groupIdentity, refs...)
	}
	payload.ReceiverRef = digestFields(sourceID, fields.Receiver)
	groupFields := append([]string{sourceID, fields.Receiver}, groupIdentity...)
	payload.GroupRef = digestFields(groupFields...)
	// GroupRef relates lifecycle phases without disclosing receiver or groupKey.
	// The queue still tracks firing and resolved messages independently.
	dedupFields := append([]string{sourceID, fields.Receiver, env.EventType}, groupIdentity...)
	env.DedupKey = digestFields(dedupFields...)
	var err error
	env.Payload, err = json.Marshal(payload)
	return env, policies, err
}

func selectPolicies(routing Routing, env Envelope, labels map[string]string) []Policy {
	seen := map[string]bool{}
	var result []Policy
	add := func(r Rule) {
		result = append(result, Policy{Role: r.Role, RouteID: r.Name, ConfigVersion: routing.ConfigVersion,
			TTL: r.TTL, Priority: r.Priority, MaxAttempts: r.MaxAttempts})
		seen[r.Role] = true
	}
	for _, r := range routing.Routes {
		if !seen[r.Role] && matchGroup(r, env, labels) {
			add(r)
		}
	}
	if len(result) == 0 {
		add(routing.Fallback)
	}
	// Concurrent projections acquire per-role queue rows in the same order.
	sort.Slice(result, func(i, j int) bool { return result[i].Role < result[j].Role })
	return result
}

func matchGroup(r Rule, env Envelope, labels map[string]string) bool {
	if (r.Source != "" && r.Source != env.Source) || (r.SourceID != "" && r.SourceID != env.SourceID) ||
		(r.EventType != "" && r.EventType != env.EventType) || (r.Severity != "" && r.Severity != env.Severity) {
		return false
	}
	for key, value := range r.Labels {
		if v, ok := labels[key]; !ok || v != value {
			return false
		}
	}
	return true
}

// Project one batch under its row lock. The queue writes and outbox deletion share a
// transaction. A failed fan-out is rolled back to a savepoint before scheduling retry.
func (p *Projector) projectOne(ctx context.Context) (bool, error) {
	var found bool
	err := p.store.WithTx(ctx, func(tx pgx.Tx) error {
		var id int64
		var accepted time.Time
		var snapshot, routes json.RawMessage
		var failures int
		err := tx.QueryRow(ctx, `SELECT b.id, b.created_at, b.envelope, b.policies, b.failures
			FROM agent_batches b WHERE b.status='pending' AND b.visible_at <= clock_timestamp()
			AND NOT EXISTS (SELECT 1 FROM agent_batches prior WHERE prior.status='pending'
				AND prior.dedup_key=b.dedup_key AND prior.id<b.id)
			ORDER BY b.visible_at,b.id LIMIT 1 FOR UPDATE OF b SKIP LOCKED`).Scan(&id, &accepted, &snapshot, &routes, &failures)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		err = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			var env Envelope
			var policies []Policy
			if err := json.Unmarshal(snapshot, &env); err != nil {
				return err
			}
			if err := json.Unmarshal(routes, &policies); err != nil {
				return err
			}
			if env.SchemaVersion != 1 || env.TraceID == "" || len(policies) == 0 {
				return errors.New("invalid analysis batch snapshot")
			}
			for _, policy := range policies {
				policy.EnqueuedAt = accepted
				if err := p.queue.Enqueue(ctx, sp, env, policy); err != nil {
					return err
				}
			}
			_, err := sp.Exec(ctx, "DELETE FROM agent_batches WHERE id=$1", id)
			return err
		})
		if err == nil {
			return nil
		}
		// Do not log error details containing labels, payloads, or SQL arguments.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failures++
		p.log.Warn("agent batch projection failed", "batch_id", id, "failures", failures)
		if failures >= 10 {
			_, err = tx.Exec(ctx, `UPDATE agent_batches SET status='dead', failures=$2,
				raw_webhook=NULL, failed_at=clock_timestamp() WHERE id=$1`, id, failures)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE agent_batches SET failures=$2,
			visible_at=clock_timestamp()+make_interval(secs => $3) WHERE id=$1`, id, failures, min(1<<(failures-1), 60))
		return err
	})
	return found, err
}

func (p *Projector) Run(ctx context.Context) {
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()
	for ctx.Err() == nil {
		select {
		case <-sweep.C:
			_, err := p.store.Pool.Exec(ctx, `DELETE FROM agent_batches WHERE id IN (
				SELECT id FROM agent_batches WHERE status='dead' AND failed_at < now()-interval '7 days'
				ORDER BY failed_at LIMIT 100 FOR UPDATE SKIP LOCKED)`)
			if err != nil && ctx.Err() == nil {
				p.log.Warn("agent batch retention failed")
			}
		default:
		}
		found, err := p.projectOne(ctx)
		if err != nil && ctx.Err() == nil {
			p.log.Warn("agent batch worker unavailable")
		}
		if found && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
