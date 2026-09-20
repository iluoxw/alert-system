package impact

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

func TestClassifyAutomatedImpact_DecommissionedPotentialRuledOut(t *testing.T) {
	node := domain.ResourceNode{
		ID: "app-1", Layer: domain.EntityLayerApplication,
		Kind:           domain.ResourceNodeKindBusinessSystem,
		LifecycleState: domain.EntityLifecycleDecommissioned,
	}
	state, rationale, confidence := classifyAutomatedImpact(node, 2, false, false)
	if state != domain.ImpactStateRuledOut || rationale != decommissionedImpactRationale || confidence != decommissionedImpactConfidence {
		t.Fatalf("下线实体自动结论应为 ruled_out / 实体已下线: state=%s rationale=%q conf=%v", state, rationale, confidence)
	}
}

func TestClassifyAutomatedImpact_DecommissionedWinsOverDirectEvidence(t *testing.T) {
	node := domain.ResourceNode{
		ID: "app-1", Layer: domain.EntityLayerApplication,
		LifecycleState: domain.EntityLifecycleDecommissioned,
	}
	state, rationale, _ := classifyAutomatedImpact(node, 0, true, true)
	if state != domain.ImpactStateRuledOut || rationale != decommissionedImpactRationale {
		t.Fatalf("下线优先于 stale/直接证据: state=%s rationale=%q", state, rationale)
	}
}

func TestClassifyAutomatedImpact_ActiveUnchanged(t *testing.T) {
	node := domain.ResourceNode{
		ID: "app-1", Layer: domain.EntityLayerApplication,
		LifecycleState: domain.EntityLifecycleActive,
	}
	state, _, conf := classifyAutomatedImpact(node, 1, false, false)
	if state != domain.ImpactStatePotential || conf != 0.65 {
		t.Fatalf("active 无证据应为 potential/0.65: state=%s conf=%v", state, conf)
	}
	state, _, conf = classifyAutomatedImpact(node, 1, true, false)
	if state != domain.ImpactStatePotential || conf != 0.4 {
		t.Fatalf("stale 应为 potential/0.4: state=%s conf=%v", state, conf)
	}
	state, _, conf = classifyAutomatedImpact(node, 0, false, true)
	if state != domain.ImpactStateConfirmed || conf != 0.95 {
		t.Fatalf("直接证据应为 confirmed/0.95: state=%s conf=%v", state, conf)
	}
}

type fakeTopology struct {
	radius []domain.BlastRadiusNode
}

func (f *fakeTopology) ListBlastRadiusByNodeID(context.Context, pgx.Tx, string, int, time.Duration) ([]domain.BlastRadiusNode, error) {
	return f.radius, nil
}

func (f *fakeTopology) ListDependencyProvidersByNodeID(context.Context, pgx.Tx, string, int, time.Duration) ([]domain.BlastRadiusNode, error) {
	return nil, nil
}

type fakeFaults struct {
	human     map[string]domain.FaultImpact
	rows      map[string]domain.FaultImpact
	replaced  []domain.FaultImpact
	upserted  []domain.FaultImpact
	related   []domain.Fault
	hasDirect bool
}

func (f *fakeFaults) HasActiveDirectEvidenceForFault(context.Context, pgx.Tx, string, time.Time) (bool, error) {
	return f.hasDirect, nil
}
func (f *fakeFaults) HasActiveDirectEvidenceForNode(context.Context, pgx.Tx, string, time.Time) (bool, error) {
	return f.hasDirect, nil
}
func (f *fakeFaults) ListForNode(context.Context, pgx.Tx, string, int) ([]domain.Fault, error) {
	if f.related == nil {
		return []domain.Fault{}, nil
	}
	return f.related, nil
}
func (f *fakeFaults) ListOpenForNodeIDs(context.Context, pgx.Tx, []string, int) ([]domain.Fault, error) {
	return []domain.Fault{}, nil
}

func (f *fakeFaults) ReplaceAutomatedImpacts(_ context.Context, _ pgx.Tx, _ string, impacts []domain.FaultImpact) error {
	f.replaced = append([]domain.FaultImpact(nil), impacts...)
	next := map[string]domain.FaultImpact{}
	for nodeID, impact := range f.rows {
		if impact.DecisionOrigin == domain.DecisionOriginHuman {
			next[nodeID] = impact
		}
	}
	for _, impact := range impacts {
		if impact.DecisionOrigin == domain.DecisionOriginHuman {
			return errHumanOrigin
		}
		if existing, ok := next[impact.NodeID]; ok && existing.DecisionOrigin == domain.DecisionOriginHuman {
			continue
		}
		impact.DecisionOrigin = domain.DecisionOriginAutomation
		next[impact.NodeID] = impact
	}
	f.rows = next
	return nil
}

func (f *fakeFaults) UpsertAutomatedImpact(_ context.Context, _ pgx.Tx, _ string, impact domain.FaultImpact) error {
	f.upserted = append(f.upserted, impact)
	if existing, ok := f.rows[impact.NodeID]; ok && existing.DecisionOrigin == domain.DecisionOriginHuman {
		return nil
	}
	impact.DecisionOrigin = domain.DecisionOriginAutomation
	if f.rows == nil {
		f.rows = map[string]domain.FaultImpact{}
	}
	f.rows[impact.NodeID] = impact
	return nil
}

var errHumanOrigin = errString("projector must not emit human decision_origin")

type errString string

func (e errString) Error() string { return string(e) }

func TestRebuildFault_DecommissionedEmitsAutomatedRuledOut(t *testing.T) {
	app := domain.ResourceNode{
		ID: "app-1", Layer: domain.EntityLayerApplication,
		Kind:           domain.ResourceNodeKindBusinessSystem,
		LifecycleState: domain.EntityLifecycleDecommissioned,
	}
	faults := &fakeFaults{rows: map[string]domain.FaultImpact{}}
	p := &Projector{
		topology: &fakeTopology{radius: []domain.BlastRadiusNode{{
			Node: app, Depth: 0, Path: []string{app.ID},
		}}},
		faults:     faults,
		maxDepth:   8,
		staleAfter: time.Hour,
	}
	if err := p.RebuildFault(context.Background(), nil, domain.Fault{
		ID: "f1", ResourceNodeID: app.ID, FirstDetected: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if len(faults.replaced) != 1 {
		t.Fatalf("应写入 1 条自动 Impact, got %d", len(faults.replaced))
	}
	got := faults.replaced[0]
	if got.State != domain.ImpactStateRuledOut || got.Rationale != decommissionedImpactRationale ||
		got.DecisionOrigin != domain.DecisionOriginAutomation {
		t.Fatalf("自动降级结果不对: %+v", got)
	}
}

func TestRebuildFault_HumanDecisionNotOverwritten(t *testing.T) {
	app := domain.ResourceNode{
		ID: "app-1", Layer: domain.EntityLayerApplication,
		Kind:           domain.ResourceNodeKindBusinessSystem,
		LifecycleState: domain.EntityLifecycleDecommissioned,
	}
	human := domain.FaultImpact{
		NodeID: app.ID, State: domain.ImpactStateConfirmed,
		Rationale: "人工确认业务仍受影响", Confidence: 1,
		DecisionOrigin: domain.DecisionOriginHuman, DecidedBy: "sre-1",
	}
	faults := &fakeFaults{rows: map[string]domain.FaultImpact{app.ID: human}}
	p := &Projector{
		topology: &fakeTopology{radius: []domain.BlastRadiusNode{{
			Node: app, Depth: 0, Path: []string{app.ID},
		}}},
		faults:     faults,
		maxDepth:   8,
		staleAfter: time.Hour,
	}
	if err := p.RebuildFault(context.Background(), nil, domain.Fault{
		ID: "f1", ResourceNodeID: app.ID, FirstDetected: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.faults.UpsertAutomatedImpact(context.Background(), nil, "f1", decommissionedAutomatedImpact(app.ID)); err != nil {
		t.Fatal(err)
	}
	kept := faults.rows[app.ID]
	if kept.DecisionOrigin != domain.DecisionOriginHuman || kept.State != domain.ImpactStateConfirmed || kept.DecidedBy != "sre-1" {
		t.Fatalf("人工 confirmed 被覆盖: %+v", kept)
	}
	if len(faults.replaced) != 1 || faults.replaced[0].DecisionOrigin != domain.DecisionOriginAutomation {
		t.Fatalf("自动重算必须只提交 automation 行: %+v", faults.replaced)
	}
}

func TestRuleOutDecommissionedImpacts_SkipsHumanRows(t *testing.T) {
	app := domain.ResourceNode{
		ID: "app-1", Layer: domain.EntityLayerApplication,
		Kind:           domain.ResourceNodeKindBusinessSystem,
		LifecycleState: domain.EntityLifecycleDecommissioned,
	}
	human := domain.FaultImpact{
		NodeID: app.ID, State: domain.ImpactStateRuledOut,
		Rationale: "人工排除", Confidence: 1,
		DecisionOrigin: domain.DecisionOriginHuman, DecidedBy: "sre-2",
	}
	faults := &fakeFaults{
		rows:    map[string]domain.FaultImpact{app.ID: human},
		related: []domain.Fault{{ID: "f1", ResourceNodeID: app.ID}},
	}
	p := &Projector{
		topology:   &fakeTopology{radius: []domain.BlastRadiusNode{{Node: app, Path: []string{app.ID}}}},
		faults:     faults,
		maxDepth:   8,
		staleAfter: time.Hour,
	}
	if err := p.RuleOutDecommissionedImpacts(context.Background(), nil, app); err != nil {
		t.Fatal(err)
	}
	kept := faults.rows[app.ID]
	if kept.DecisionOrigin != domain.DecisionOriginHuman || kept.Rationale != "人工排除" {
		t.Fatalf("人工 ruled_out 被覆盖: %+v", kept)
	}
}
