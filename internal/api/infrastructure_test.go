// managedEntityRequest.domain() 的纯函数校验测试：受管实体新增 aliases/dependencies
// 字段后，校验规则应与 serviceRequest.domain()（service_catalog.go）保持一致（空值、
// 同一实体内 alias 重复、依赖目标重复、UUID 格式、criticality 合法）。不依赖数据库，
// 因为 RegisterInfrastructureRoutes 接收的是具体的 *store.TopologyRepo，走完整 HTTP
// 需要真实 PG（见 internal/store/infrastructure_entities_test.go 的集成测试），这里
// 只覆盖请求解析/校验本身。
package api

import (
	"testing"

	"alert-system/internal/domain"
)

func boolPtr(v bool) *bool { return &v }

func validManagedEntityRequest() managedEntityRequest {
	return managedEntityRequest{
		EntityKey: "host:10.61.17.11", Layer: domain.EntityLayerInfrastructure,
		Name: "10.61.17.11", Environment: "prod", OwnerTeamID: "11111111-1111-1111-1111-111111111111",
	}
}

func TestManagedEntityRequestDomain_ValidWithAliasesAndDependencies(t *testing.T) {
	request := validManagedEntityRequest()
	request.Aliases = []aliasRequest{
		{Namespace: "prometheus.instance", Value: "10.61.17.11:4646", Enabled: boolPtr(true)},
		{Namespace: "prometheus.instance", Value: "10.61.17.11:9100", Enabled: boolPtr(true)},
	}
	request.Dependencies = []dependencyRequest{
		{ToEntityID: "22222222-2222-2222-2222-222222222222", Criticality: domain.DependencyCriticalityHigh},
	}
	form, err := request.domain()
	if err != nil {
		t.Fatalf("合法请求不应报错: %v", err)
	}
	if len(form.Aliases) != 2 || len(form.Dependencies) != 1 {
		t.Fatalf("解析结果不完整: %+v", form)
	}
	if form.Dependencies[0].Relation != domain.DependencyRelationDependsOn || form.Dependencies[0].Origin != domain.RelationOriginDeclared {
		t.Fatalf("依赖 relation/origin 应固定为 depends_on/declared: %+v", form.Dependencies[0])
	}
}

func TestManagedEntityRequestDomain_RejectsBaseFieldsInvalid(t *testing.T) {
	request := validManagedEntityRequest()
	request.OwnerTeamID = "not-a-uuid"
	if _, err := request.domain(); err == nil {
		t.Fatal("非法 owner_team_id 应被拒绝")
	}

	request = validManagedEntityRequest()
	request.Layer = domain.EntityLayerApplication
	if _, err := request.domain(); err == nil {
		t.Fatal("受管实体不允许 application layer")
	}
}

func TestManagedEntityRequestDomain_RejectsDuplicateAlias(t *testing.T) {
	request := validManagedEntityRequest()
	request.Aliases = []aliasRequest{
		{Namespace: "prometheus.instance", Value: "10.61.17.11:4646"},
		{Namespace: "prometheus.instance", Value: "10.61.17.11:4646"},
	}
	if _, err := request.domain(); err == nil {
		t.Fatal("同一实体内重复 alias 应被拒绝")
	}
}

func TestManagedEntityRequestDomain_RejectsEmptyAliasFields(t *testing.T) {
	request := validManagedEntityRequest()
	request.Aliases = []aliasRequest{{Namespace: "", Value: "10.61.17.11:4646"}}
	if _, err := request.domain(); err == nil {
		t.Fatal("空 namespace 应被拒绝")
	}
}

func TestManagedEntityRequestDomain_RejectsDuplicateDependencyTarget(t *testing.T) {
	request := validManagedEntityRequest()
	target := "22222222-2222-2222-2222-222222222222"
	request.Dependencies = []dependencyRequest{
		{ToEntityID: target, Criticality: domain.DependencyCriticalityHigh},
		{ToEntityID: target, Criticality: domain.DependencyCriticalityLow},
	}
	if _, err := request.domain(); err == nil {
		t.Fatal("同一实体重复声明依赖目标应被拒绝")
	}
}

func TestManagedEntityRequestDomain_RejectsInvalidDependencyFields(t *testing.T) {
	request := validManagedEntityRequest()
	request.Dependencies = []dependencyRequest{{ToEntityID: "not-a-uuid", Criticality: domain.DependencyCriticalityHigh}}
	if _, err := request.domain(); err == nil {
		t.Fatal("非法 UUID 依赖目标应被拒绝")
	}

	request = validManagedEntityRequest()
	request.Dependencies = []dependencyRequest{{ToEntityID: "22222222-2222-2222-2222-222222222222", Criticality: "not-a-level"}}
	if _, err := request.domain(); err == nil {
		t.Fatal("非法 criticality 应被拒绝")
	}
}

func TestManagedEntityRequestDomain_DefaultsCriticalityToMedium(t *testing.T) {
	request := validManagedEntityRequest()
	request.Dependencies = []dependencyRequest{{ToEntityID: "22222222-2222-2222-2222-222222222222"}}
	form, err := request.domain()
	if err != nil {
		t.Fatalf("未显式声明 criticality 不应报错: %v", err)
	}
	if form.Dependencies[0].Criticality != domain.DependencyCriticalityMedium {
		t.Fatalf("缺省 criticality 应回落为 medium: %+v", form.Dependencies[0])
	}
}

// 字段缺失与显式空数组必须可区分：缺失 → nil（store 保持现状），[] → 非 nil 空切片（清空）。
func TestManagedEntityRequestDomain_DistinguishesAbsentFromEmptyCollections(t *testing.T) {
	absent, err := validManagedEntityRequest().domain()
	if err != nil {
		t.Fatalf("合法请求不应报错: %v", err)
	}
	if absent.Aliases != nil || absent.Dependencies != nil {
		t.Fatalf("未提供 aliases/dependencies 时应保持 nil 以表示不修改: %+v", absent)
	}

	request := validManagedEntityRequest()
	request.Aliases, request.Dependencies = []aliasRequest{}, []dependencyRequest{}
	empty, err := request.domain()
	if err != nil {
		t.Fatalf("合法请求不应报错: %v", err)
	}
	if empty.Aliases == nil || len(empty.Aliases) != 0 || empty.Dependencies == nil || len(empty.Dependencies) != 0 {
		t.Fatalf("显式 [] 应产出非 nil 空切片以表示清空: %+v", empty)
	}
}
