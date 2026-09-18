package ccloud

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

// loadTopologyData 直接解析响应信封的 data 字段，绕过 HTTP 层，专供 importer 测试
// 用真实契约样本构造 ProjectTopology/ServiceTopology。
func loadTopologyData(t *testing.T, path string) topologyData {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var env topologyEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", path, err)
	}
	return env.Data
}

func fixtureProjectTopology(t *testing.T) ProjectTopology {
	d := loadTopologyData(t, "testdata/fulllink_topology.json")
	return ProjectTopology{ProjectID: "proj-1", Nodes: d.Nodes, Edges: d.Edges}
}

func fixtureServiceTopology(t *testing.T) ServiceTopology {
	d := loadTopologyData(t, "testdata/service_topology_cplm-boot-prod.json")
	return ServiceTopology{AppID: "4f2b08e91ce2f89281397c4579f8f5f435e69985", Nodes: d.Nodes, Edges: d.Edges}
}

func noResolve(string, string) (string, bool) { return "", false }

// ---- 契约事实 → 断言：见函数级注释标注对应 README.md 章节 ----

// README §12：自环 2 条（6c66df2c→6c66df2c totalCount=18237 有流量；
// 3367c6e2→3367c6e2 totalCount=0），二者均须丢弃——"自环也可能有流量"不能作为
// 保留依据。
func TestMapTopology_SelfLoopsAlwaysDropped(t *testing.T) {
	in := fixtureProjectTopology(t)
	svc := fixtureServiceTopology(t)
	result := MapTopology(in, []ServiceTopology{svc}, noResolve)
	if result.DroppedSelfLoop != 2 {
		t.Fatalf("DroppedSelfLoop = %d, want 2", result.DroppedSelfLoop)
	}
	for _, e := range result.Edges {
		if e.FromRef == e.ToRef {
			t.Fatalf("Edges 中不应残留自环: %+v", e)
		}
	}
}

// README §12：totalCount=0 的边高占比（全链路 40/77），stale 标记是常态而非异常。
// 用真实全链路视图（不搭配 service 视图，避免跨视图去重改变计数）验证 Stale 计数。
func TestMapTopology_ZeroTotalCountMarksStale(t *testing.T) {
	in := fixtureProjectTopology(t)
	result := MapTopology(in, nil, noResolve)

	var staleCount int
	for _, e := range result.Edges {
		if e.Stale {
			staleCount++
		}
	}
	// 40 条 totalCount=0 的边里含 1 条自环（3367c6e2→3367c6e2，已被丢弃），
	// 故落地边里 stale 的应为 39 条。
	if staleCount != 39 {
		t.Fatalf("stale edges = %d, want 39", staleCount)
	}
}

// README §2/§3：key 方向在两个视图里相反（全链路 77/77 为 from->to，服务拓扑
// 12/12 为 to->from），因此 key 完全不可信；MapTopology 必须只信 from/to、直接
// 照抄不翻转，绝不参考 edge.key。用一条已知方向的真实边验证。
func TestMapTopology_DirectionTakenFromFromTo_NotKey(t *testing.T) {
	// CPLM-BOOT-PROD(from) -> REDIS(to)，totalCount=55264，只可能是应用调 Redis
	// （README §3 决定性证据）。REDIS 的 key 未知（unresolved），改用平台内部
	// App→App 的样本验证方向：89866988...(from,SERVER) -> 4f2b08e9...(to,CPLM-BOOT-PROD)。
	svc := fixtureServiceTopology(t)
	result := MapTopology(ProjectTopology{}, []ServiceTopology{svc}, noResolve)

	found := false
	for _, e := range result.Edges {
		if e.FromRef == "89866988f38d424e805fd0e9" && e.ToRef == "4f2b08e91ce2f89281397c4579f8f5f435e69985" {
			found = true
		}
		// 若方向被误翻转，会出现反向边而不是这条；两者不可能同时出现在同一批
		// 结果里（下面单独断言反向不存在）。
		if e.FromRef == "4f2b08e91ce2f89281397c4579f8f5f435e69985" && e.ToRef == "89866988f38d424e805fd0e9" {
			t.Fatalf("方向被误翻转：不应出现 4f2b08e9(App)->89866988(caller)")
		}
	}
	if !found {
		t.Fatalf("未找到方向正确的边 89866988->4f2b08e9（照抄 from/to）")
	}
}

// README §4：唯一的空 nodeType 样本是 EXTERNAL_HTTP_SERVER（serviceType 同名）；
// 分类判据必须是"nodeType 非空用它，为空回落 serviceType"，否则会漏判这个节点、
// 也就无法按规则忽略它。这里用真实全链路+服务拓扑数据验证 EXTERNAL_HTTP_SERVER
// 不产生任何边（样例中它没有任何边），再用合成边验证一旦带边会被正确丢弃并计数。
func TestMapTopology_EmptyNodeTypeFallsBackToServiceType(t *testing.T) {
	svc := fixtureServiceTopology(t)
	result := MapTopology(ProjectTopology{}, []ServiceTopology{svc}, noResolve)
	if result.DroppedExternalHTTP != 0 {
		t.Fatalf("真实样例中 EXTERNAL_HTTP_SERVER 无边，DroppedExternalHTTP 应为 0，got %d", result.DroppedExternalHTTP)
	}

	// 合成场景：一旦 EXTERNAL_HTTP_SERVER 开始带边，必须显式计数，不能被误分类
	// 成"未知"从而悄悄产生一条边或被静默丢弃却不计数。
	externalNode := TopologyNode{Key: "ext-1", ServiceName: "obj.example.com:443", ServiceType: "EXTERNAL_HTTP_SERVER", NodeType: ""}
	appNode := TopologyNode{Key: "app-1", ServiceName: "app-1", ServiceType: "SERVER", NodeType: "CCLOUD_APP"}
	in := ProjectTopology{
		Nodes: []TopologyNode{externalNode, appNode},
		Edges: []TopologyEdge{{From: "app-1", To: "ext-1", TotalCount: 5}},
	}
	result2 := MapTopology(in, nil, noResolve)
	if result2.DroppedExternalHTTP != 1 {
		t.Fatalf("DroppedExternalHTTP = %d, want 1（一旦带边必须计数）", result2.DroppedExternalHTTP)
	}
	if len(result2.Edges) != 0 {
		t.Fatalf("EXTERNAL_HTTP_SERVER 参与的边不应落地: %+v", result2.Edges)
	}
}

// README §5：serviceType 有前导空格（" POSTGRESQL"），分类判据比较前必须
// TrimSpace，否则 RELY_NODE 会被误判成"未知类型"而不是走待对齐队列逻辑。
func TestMapTopology_ServiceTypeLeadingSpaceTrimmed(t *testing.T) {
	svc := fixtureServiceTopology(t)
	result := MapTopology(ProjectTopology{}, []ServiceTopology{svc}, noResolve)

	var found *UnresolvedIdentity
	for i := range result.Unresolved {
		if result.Unresolved[i].Value == "d3d9e478a934499099a296644a8794a8" { // cplm-postgres001:5432
			found = &result.Unresolved[i]
		}
	}
	if found == nil {
		t.Fatalf("未找到 POSTGRESQL RELY_NODE 的 Unresolved 记录")
	}
	if found.ServiceType != "POSTGRESQL" {
		t.Fatalf("ServiceType = %q, want TrimSpace 后的 POSTGRESQL", found.ServiceType)
	}
}

// README §6：serviceName 不能作为身份——lcp-app 在服务拓扑视图里出现 2 次，是
// 两个不同应用（key 6f158bfe... 与 f8b2cda6...），绝不能因同名被误合并成一个。
func TestMapTopology_SameServiceNameDifferentKeysNotMerged(t *testing.T) {
	svc := fixtureServiceTopology(t)
	// 真实样例中只有 f8b2cda6 这个 lcp-app 实例有边，6f158bfe 是孤立节点；
	// 补一条合成边让两个同名节点都参与拓扑，验证各自独立产生边、不合并。
	svc.Edges = append(svc.Edges, TopologyEdge{From: "6f158bfe4048489996fdfd6c", To: "4f2b08e91ce2f89281397c4579f8f5f435e69985", TotalCount: 1})

	result := MapTopology(ProjectTopology{}, []ServiceTopology{svc}, noResolve)
	refs := map[string]bool{}
	for _, e := range result.Edges {
		if e.ToRef == "4f2b08e91ce2f89281397c4579f8f5f435e69985" {
			refs[e.FromRef] = true
		}
	}
	if !refs["6f158bfe4048489996fdfd6c"] || !refs["f8b2cda666e249d68cb37d65"] {
		t.Fatalf("两个同名 lcp-app 节点应各自产生独立边，got refs=%v", refs)
	}
}

// README §7：appId 同时存在 24 与 40 字符两种长度，两个 fixture 内都混合存在；
// 分类与边生成不得按长度/格式过滤，两种长度的 CCLOUD_APP 都必须被正常处理。
func TestMapTopology_AppIDLengthNotFiltered(t *testing.T) {
	in := fixtureProjectTopology(t)
	svc := fixtureServiceTopology(t)
	result := MapTopology(in, []ServiceTopology{svc}, noResolve)

	lens := map[int]bool{}
	for _, e := range result.Edges {
		lens[len(e.FromRef)] = true
		lens[len(e.ToRef)] = true
	}
	if !lens[24] || !lens[40] {
		t.Fatalf("24/40 两种长度的 appId 应均出现在落地边里，got lens=%v", lens)
	}
}

// README §8：health 的 null 与 0.0 语义不同（0.0 是真实测量值，如 APISIX
// nginx-prd-it）。MapTopology 本身不使用 health 做任何判定（那是 P4 健康证据的
// 职责），这里只验证 DTO 层面的两种值都不会导致节点被跳过——通过验证 APISIX 节点
// 仍然正常参与拓扑分类（不会被 health=0.0 误判为"无效/缺失"而丢弃）。
func TestMapTopology_HealthNullAndZeroBothParticipate(t *testing.T) {
	in := fixtureProjectTopology(t)
	result := MapTopology(in, nil, noResolve)
	// chang_ting_waf(health=null) 与 nginx-prd-it(health=0.0) 都是 APISIX/WAF
	// 节点、在样例边里作为入口(from)出现；验证二者所在的边都被正常落地（不因为
	// health 取值被跳过）。
	var sawWAFEdge, sawAPISIXEdge bool
	for _, e := range result.Edges {
		if e.FromRef == "chang_ting_waf" {
			sawWAFEdge = true
		}
		if e.FromRef == "60-kube-system-nginx-prd-it" {
			sawAPISIXEdge = true
		}
	}
	if !sawWAFEdge || !sawAPISIXEdge {
		t.Fatalf("health=null(WAF) 与 health=0.0(APISIX) 所在的边都应正常落地: waf=%v apisix=%v", sawWAFEdge, sawAPISIXEdge)
	}
}

// README §9：coreMetrics 形状不稳定（描述文案、顺序、单位在两视图间都不一致），
// 一概不解析、不参与任何判定。TopologyNode 结构体本身就没有 CoreMetrics 字段，
// 用反射断言 DTO 层面确实没有暴露这个字段，防止未来被误加回来参与判定。
func TestTopologyNode_DoesNotExposeCoreMetrics(t *testing.T) {
	typ := reflect.TypeOf(TopologyNode{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Name == "CoreMetrics" {
			t.Fatalf("TopologyNode 不应包含 CoreMetrics 字段（两视图量纲/顺序/大小写均不一致，不得参与判定）")
		}
	}
}

// README §11：":0" 这类退化 host:port 必须用 node.key 锚定进 Unresolved，value
// 不能是 host:port（否则多个不同实例的 ":0" Redis 会塌进同一主键行）。
func TestMapTopology_ColonZeroRedisGoesUnresolvedByKey(t *testing.T) {
	svc := fixtureServiceTopology(t)
	result := MapTopology(ProjectTopology{}, []ServiceTopology{svc}, noResolve)

	var found *UnresolvedIdentity
	for i := range result.Unresolved {
		if result.Unresolved[i].Value == "308e2cc521d64075b181a08e6059cb5b" {
			found = &result.Unresolved[i]
		}
	}
	if found == nil {
		t.Fatalf("未找到 :0 Redis 的 Unresolved 记录（value 应为 node.key）")
	}
	if found.HostPort != ":0" {
		t.Errorf("HostPort = %q, want :0（仅展示，不作主键）", found.HostPort)
	}
	if found.ServiceType != "REDIS" {
		t.Errorf("ServiceType = %q, want REDIS", found.ServiceType)
	}
	if found.SampleAppID != "4f2b08e91ce2f89281397c4579f8f5f435e69985" {
		t.Errorf("SampleAppID = %q, want 发起该服务拓扑查询的 appId", found.SampleAppID)
	}
}

// R1.2：未对齐 RELY_NODE 不论是否已是某条边的端点都必须写入待对齐队列——真实
// 样例里 5 个 RELY_NODE 中有 3 个当前没有任何边（POSTGRESQL:1921、两条 ORACLE），
// 若只在处理边时才判定会把它们完全漏掉。
func TestMapTopology_IsolatedRelyNodesStillUnresolved(t *testing.T) {
	svc := fixtureServiceTopology(t)
	result := MapTopology(ProjectTopology{}, []ServiceTopology{svc}, noResolve)

	values := map[string]bool{}
	for _, u := range result.Unresolved {
		values[u.Value] = true
	}
	wantIsolated := []string{
		"004f58691a7340cdbf90bd1044ac53f7", // postgres002:1921，无边
		"718ebf28ed24407d8fdfba5eda09c57e", // dm02 oracle，无边
		"b275629dcc1d450080d3055c9c74fe40", // dm03 oracle，无边
	}
	for _, v := range wantIsolated {
		if !values[v] {
			t.Errorf("孤立 RELY_NODE %s 应出现在 Unresolved 中", v)
		}
	}
	if len(result.Unresolved) != 5 {
		t.Fatalf("Unresolved 数量 = %d, want 5（该 fixture 全部 RELY_NODE 均未对齐）", len(result.Unresolved))
	}
}

// R1.1/README §11：RELY_NODE 用 node.key 命中已有别名时应产生边并指向该实体，
// 不再进入 Unresolved；同一命名空间下 host:port 命中也应生效（人工可能已经用
// host:port 建立别名）。
func TestMapTopology_RelyNodeResolvedByAlias(t *testing.T) {
	svc := fixtureServiceTopology(t)
	const redisEntityID = "entity-redis-1"
	resolve := func(namespace, value string) (string, bool) {
		if namespace == AliasNamespaceRelyNode && value == "308e2cc521d64075b181a08e6059cb5b" {
			return redisEntityID, true
		}
		return "", false
	}
	result := MapTopology(ProjectTopology{}, []ServiceTopology{svc}, resolve)

	for _, u := range result.Unresolved {
		if u.Value == "308e2cc521d64075b181a08e6059cb5b" {
			t.Fatalf("已 resolve 命中的 RELY_NODE 不应出现在 Unresolved 中: %+v", u)
		}
	}
	var found bool
	for _, e := range result.Edges {
		if e.ToRef == "308e2cc521d64075b181a08e6059cb5b" {
			found = true
			if e.ToEntityID != redisEntityID {
				t.Errorf("ToEntityID = %q, want %q", e.ToEntityID, redisEntityID)
			}
		}
	}
	if !found {
		t.Fatalf("resolve 命中的 RELY_NODE 应正常产生边")
	}
}

// R1.7：CCLOUD_APP 已有 service_registrations 登记时，alias 应合并挂到已有实体
// （ResolvedApps 命中），不新建；未登记的应用不出现在 ResolvedApps 中，保持
// unmanaged 待认领语义（缺席即代表未登记，不建团队、不改归属）。
func TestMapTopology_RegisteredAppMergesAlias_UnregisteredStaysUnmanaged(t *testing.T) {
	svc := fixtureServiceTopology(t)
	const registeredEntityID = "entity-cplm-boot-prod"
	resolve := func(namespace, value string) (string, bool) {
		if namespace == AliasNamespaceAppID && value == "4f2b08e91ce2f89281397c4579f8f5f435e69985" {
			return registeredEntityID, true
		}
		return "", false
	}
	result := MapTopology(ProjectTopology{}, []ServiceTopology{svc}, resolve)

	if got := result.ResolvedApps["4f2b08e91ce2f89281397c4579f8f5f435e69985"]; got != registeredEntityID {
		t.Errorf("ResolvedApps[已登记应用] = %q, want %q", got, registeredEntityID)
	}
	if _, ok := result.ResolvedApps["f8b2cda666e249d68cb37d65"]; ok {
		t.Errorf("未登记应用不应出现在 ResolvedApps 中")
	}
	for _, e := range result.Edges {
		if e.ToRef == "4f2b08e91ce2f89281397c4579f8f5f435e69985" {
			if e.ToEntityID != registeredEntityID {
				t.Errorf("已登记应用作为边端点时 ToEntityID 应指向既有实体, got %q", e.ToEntityID)
			}
		}
		if e.FromRef == "f8b2cda666e249d68cb37d65" && e.FromEntityID != "" {
			t.Errorf("未登记应用不应带 FromEntityID, got %q", e.FromEntityID)
		}
	}
}

// README §3/§12：全链路视图与服务拓扑视图对同一 App→App 关系有重叠观测
// （如 6c66df2c…→4f2b08e9… 两视图 totalCount 均为 2414），MapTopology 必须按
// (from,to) 去重合并，不能重复产生两条边。
func TestMapTopology_OverlappingEdgesAcrossViewsDeduped(t *testing.T) {
	in := fixtureProjectTopology(t)
	svc := fixtureServiceTopology(t)
	result := MapTopology(in, []ServiceTopology{svc}, noResolve)

	seen := map[[2]string]int{}
	for _, e := range result.Edges {
		seen[[2]string{e.FromRef, e.ToRef}]++
	}
	for k, count := range seen {
		if count > 1 {
			t.Fatalf("(from,to)=%v 重复出现 %d 次，应去重合并", k, count)
		}
	}
	// 两视图交叉验证的样本关系必须存在且只出现一次。
	if seen[[2]string{"6c66df2cac4e541e7e2b78f6dd9641559dc6c32f", "4f2b08e91ce2f89281397c4579f8f5f435e69985"}] != 1 {
		t.Fatalf("跨视图重叠边未被正确去重")
	}
}

// 全角项目名（PRJ-INFO-PROD（cplm-ipm）served as serviceName）不应影响处理——
// 分类与边生成只看 key，不看 serviceName，全角字符不应导致 panic 或被过滤。
func TestMapTopology_FullWidthServiceNameDoesNotBreakProcessing(t *testing.T) {
	in := fixtureProjectTopology(t)
	result := MapTopology(in, nil, noResolve)
	var found bool
	for _, n := range in.Nodes {
		if n.Key == "0925d8038db09b474fcfda0ccaa1651dcd3aa630" {
			found = true
			if n.ServiceName == "" {
				t.Fatalf("fixture 缺少全角项目名样本，无法验证")
			}
		}
	}
	if !found {
		t.Fatalf("fixture 中未找到全角项目名节点，样本失效")
	}
	// 该节点作为 CCLOUD_APP 若参与边，应正常落地（不因 serviceName 含全角字符异常）。
	for _, e := range result.Edges {
		if e.FromRef == "0925d8038db09b474fcfda0ccaa1651dcd3aa630" || e.ToRef == "0925d8038db09b474fcfda0ccaa1651dcd3aa630" {
			return // 找到即通过：证明该节点被正常处理并产生了边
		}
	}
}

// 同输入重放结果一致（幂等）：两次调用完全相同的输入必须得到 reflect.DeepEqual
// 相等的结果，不依赖 map 迭代顺序等不确定性。
func TestMapTopology_IdempotentAcrossReplays(t *testing.T) {
	in := fixtureProjectTopology(t)
	svc := fixtureServiceTopology(t)

	r1 := MapTopology(in, []ServiceTopology{svc}, noResolve)
	r2 := MapTopology(in, []ServiceTopology{svc}, noResolve)
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("重放同一份输入结果不一致:\nr1=%+v\nr2=%+v", r1, r2)
	}

	// 额外验证输出顺序本身是确定性排序（不是恰好两次 map 迭代顺序相同）。
	sortedEdges := append([]Edge(nil), r1.Edges...)
	sort.Slice(sortedEdges, func(i, j int) bool {
		if sortedEdges[i].FromRef != sortedEdges[j].FromRef {
			return sortedEdges[i].FromRef < sortedEdges[j].FromRef
		}
		return sortedEdges[i].ToRef < sortedEdges[j].ToRef
	})
	if !reflect.DeepEqual(sortedEdges, r1.Edges) {
		t.Fatalf("Edges 输出未按 (FromRef,ToRef) 排序，重放一致性依赖了不确定的 map 顺序")
	}
}

// nil resolve 不应 panic（NewClient 装配失败/调用方未注入时的防御）。
func TestMapTopology_NilResolverDoesNotPanic(t *testing.T) {
	svc := fixtureServiceTopology(t)
	result := MapTopology(ProjectTopology{}, []ServiceTopology{svc}, nil)
	if len(result.Unresolved) == 0 {
		t.Fatalf("nil resolver 应等价于恒不命中，仍应产生 Unresolved")
	}
}
