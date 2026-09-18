package ccloud

import (
	"sort"
	"strconv"
	"strings"
)

// 别名命名空间常量，与 R1.1/testdata/README.md §11 的裁决一致：CCLOUD_APP 用
// node.key（恒等于 metadata.appId）对齐；RELY_NODE 以 node.key 为主锚点，host:port
// 只是同一命名空间下便于人识别的第二个 value，二者共用 "ccloud.rely_node"。
const (
	AliasNamespaceAppID    = "ccloud.app_id"
	AliasNamespaceRelyNode = "ccloud.rely_node"
)

// nodeType 分类字面量。取值直接来自平台响应，见 testdata/README.md §4。
const (
	nodeTypeCCloudApp    = "CCLOUD_APP"
	nodeTypeAPISIX       = "APISIX"
	nodeTypeWAF          = "WAF"
	nodeTypeRelyNode     = "RELY_NODE"
	nodeTypeExternalHTTP = "EXTERNAL_HTTP_SERVER"
)

// AliasResolver 由调用方（未来 task 9 的 syncer）注入，桥接 store 对 entity_aliases
// 的查询，使本文件保持纯函数、不依赖 store（不变量 15/23）。命中时返回已存在的
// canonical 实体 ID。
type AliasResolver func(namespace, value string) (entityID string, ok bool)

// UnresolvedIdentity 是待对齐队列（迁移 000027 unresolved_identities，任务 7）的
// 内存态表示。Value 恒为 node.key，不能用 host:port——多个 RELY_NODE 都报 ":0" 时
// 会塌进同一主键行互相覆盖（testdata/README.md §11）。HostPort/ServiceType 只作
// 展示，SampleAppID 记录发现该未对齐值时所属的应用，供人判断上下文。
type UnresolvedIdentity struct {
	Namespace   string
	Value       string
	HostPort    string
	ServiceType string
	SampleAppID string
}

// Edge 是 MapTopology 的边输出。domain 包目前还没有专门的 TopologySnapshotEdge
// 类型（design.md 把新增 ToNodeID 字段列为任务 8 的扩展点），本任务只交付纯映射，
// 因此先用 ccloud 包内类型承载结果，任务 8/9 落库时再转换，避免越权改动 domain。
//
// FromRef/ToRef 恒为 node.key，供落库时按 external_ref 查找/新建 unmanaged 节点；
// FromEntityID/ToEntityID 非空时表示该端已经 resolve 命中既有 canonical 实体
// （RELY_NODE 用 node.key/host:port 锚定命中，或 CCLOUD_APP 命中 service_registrations
// 登记），此时应直接指向该实体、不再新建（不变量 15）。
type Edge struct {
	FromRef      string
	ToRef        string
	FromEntityID string
	ToEntityID   string
	Stale        bool // totalCount == 0（testdata/README.md §12：占比很高，是常态而非异常）
}

// ImportResult 是 MapTopology 的返回值。
//
// ResolvedApps 是 design.md 未列出但任务描述明确要求可测的信息：CCLOUD_APP 的
// node.key -> 已存在 canonical 实体 ID，仅当该应用命中 service_registrations 登记
// （即 resolve(AliasNamespaceAppID, key) 命中）时才有条目；未登记的应用不出现在
// 这里，语义即"保留 unmanaged 待认领"（R1.7）。
type ImportResult struct {
	Edges               []Edge
	Unresolved          []UnresolvedIdentity
	ResolvedApps        map[string]string
	DroppedSelfLoop     int
	DroppedExternalHTTP int
}

// nodeClass 是节点分类判据的结果，只用于本文件内部的边处理分支。
type nodeClass int

const (
	classUnknown nodeClass = iota
	classApp
	classPlatform
	classRelyNode
	classExternalHTTP
)

// classify 判定节点类型：nodeType 非空时用它，为空回落 serviceType（唯一的空
// nodeType 样本是 EXTERNAL_HTTP_SERVER，见 testdata/README.md §4）；比较前一律
// TrimSpace（serviceType 有前导空格，如 " POSTGRESQL"）。
func classify(n TopologyNode) nodeClass {
	t := strings.TrimSpace(n.NodeType)
	if t == "" {
		t = n.TrimmedServiceType()
	}
	switch t {
	case nodeTypeCCloudApp:
		return classApp
	case nodeTypeAPISIX, nodeTypeWAF:
		return classPlatform
	case nodeTypeRelyNode:
		return classRelyNode
	case nodeTypeExternalHTTP:
		return classExternalHTTP
	default:
		return classUnknown
	}
}

// isLegalHostPort 判定 serviceName 是否是一个可用于识别的 host:port：host 非空、
// port 非空且为正整数。":0" 这类退化值（host 为空、port 为 0）不合法——它不具备
// 跨实例区分能力，用作 alias value 会让多个不同 RELY_NODE 互相覆盖
// （testdata/README.md §11）。
func isLegalHostPort(s string) bool {
	idx := strings.LastIndex(s, ":")
	if idx <= 0 || idx == len(s)-1 {
		return false
	}
	host, port := s[:idx], s[idx+1:]
	if strings.TrimSpace(host) == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0
}

// MapTopology 把 ccloud 拓扑响应映射为待落库的边与待对齐身份，纯函数、无 IO、
// 不 import store。in 是项目级全链路视图，svc 是按应用逐个拉取的服务拓扑视图
// （全链路视图没有 RELY_NODE，见 testdata/README.md §1，因此二者都需要）。
func MapTopology(in ProjectTopology, svc []ServiceTopology, resolve AliasResolver) ImportResult {
	if resolve == nil {
		resolve = func(string, string) (string, bool) { return "", false }
	}

	// 合并全部来源的节点，供边处理时按 key 反查分类。多个来源出现同一 key 时
	// 后来源覆盖前者——实测数据里同一节点在两个视图中的分类字段一致，不构成冲突。
	nodes := make(map[string]TopologyNode, len(in.Nodes))
	for _, n := range in.Nodes {
		nodes[n.Key] = n
	}
	for _, s := range svc {
		for _, n := range s.Nodes {
			nodes[n.Key] = n
		}
	}

	// appSample 记录每个 RELY_NODE/EXTERNAL_HTTP_SERVER 节点最早在哪个应用的服务
	// 拓扑视图里被观测到，用于 Unresolved.SampleAppID（全链路视图不含这两类节点，
	// 只有 svc 遍历会命中）。
	appSample := make(map[string]string)
	for _, s := range svc {
		for _, n := range s.Nodes {
			if _, ok := appSample[n.Key]; !ok {
				appSample[n.Key] = s.AppID
			}
		}
	}

	resolvedApps := make(map[string]string)
	unresolvedByValue := make(map[string]*UnresolvedIdentity)
	unresolvedOrder := make([]string, 0)
	edgeAgg := make(map[[2]string]*Edge)
	edgeOrder := make([][2]string, 0)
	var droppedSelfLoop, droppedExternalHTTP int

	// relyResolved/relyHit 缓存每个 RELY_NODE key 的 resolve 结果，避免节点级
	// 预扫描与边处理两处重复调用 resolve 并重复判定。
	relyResolved := make(map[string]string)
	relyHit := make(map[string]bool)

	// resolveRelyNode 对单个 RELY_NODE 节点只求解一次：先按 node.key 锚定
	// （AliasNamespaceRelyNode），未命中且 host:port 合法时再退一步按 host:port
	// 查（同一命名空间，见 R1.1/testdata/README.md §11）；仍未命中则记入
	// Unresolved——按 key 去重，第一次遇到时即写入。
	resolveRelyNode := func(n TopologyNode) (entityID string, ok bool) {
		if v, seen := relyHit[n.Key]; seen {
			return relyResolved[n.Key], v
		}
		if id, hit := resolve(AliasNamespaceRelyNode, n.Key); hit {
			relyResolved[n.Key], relyHit[n.Key] = id, true
			return id, true
		}
		if isLegalHostPort(n.ServiceName) {
			if id, hit := resolve(AliasNamespaceRelyNode, n.ServiceName); hit {
				relyResolved[n.Key], relyHit[n.Key] = id, true
				return id, true
			}
		}
		relyHit[n.Key] = false
		unresolvedByValue[n.Key] = &UnresolvedIdentity{
			Namespace:   AliasNamespaceRelyNode,
			Value:       n.Key,
			HostPort:    n.ServiceName,
			ServiceType: n.TrimmedServiceType(),
			SampleAppID: appSample[n.Key],
		}
		unresolvedOrder = append(unresolvedOrder, n.Key)
		return "", false
	}

	// resolveApp 对单个 CCLOUD_APP 节点只求解一次：命中 service_registrations
	// 登记（AliasNamespaceAppID）时记入 ResolvedApps，供调用方合并 alias 到既有
	// 实体（R1.7）；未命中不记录任何东西——缺席即代表"保留 unmanaged 待认领"。
	appHit := make(map[string]bool)
	resolveApp := func(n TopologyNode) (entityID string, ok bool) {
		if v, seen := appHit[n.Key]; seen {
			return resolvedApps[n.Key], v
		}
		if id, hit := resolve(AliasNamespaceAppID, n.Key); hit {
			resolvedApps[n.Key] = id
			appHit[n.Key] = true
			return id, true
		}
		appHit[n.Key] = false
		return "", false
	}

	// 节点级预扫描：R1.2/R1.7 都是对"某 RELY_NODE/CCLOUD_APP 节点"的陈述，不限定
	// 该节点必须是某条边的端点——实测数据里 5 个 RELY_NODE 中有 3 个当前没有任何
	// 边（testdata/service_topology_cplm-boot-prod.json），若只在处理边时才判定，
	// 这 3 个会被完全漏掉、永远进不了待对齐队列。因此先对全部节点跑一遍判定，
	// 边处理阶段复用同一份缓存结果，不重复判定。
	for _, n := range nodes {
		switch classify(n) {
		case classRelyNode:
			resolveRelyNode(n)
		case classApp:
			resolveApp(n)
		}
	}

	// resolveEndpoint 返回该端点是否可以作为边端点使用（ref, entityID, ok）。
	// classApp/classPlatform 总是可用（未登记时走既有的 unmanaged 语义，不在本函数
	// 内建实体）；classRelyNode 必须 resolve 命中才可用，未命中的已在预扫描阶段
	// 记入待对齐队列；classExternalHTTP/classUnknown 一律不可用。
	resolveEndpoint := func(n TopologyNode) (ref, entityID string, ok bool) {
		switch classify(n) {
		case classApp:
			id, _ := resolveApp(n)
			return n.Key, id, true
		case classPlatform:
			return n.Key, "", true
		case classRelyNode:
			if id, hit := resolveRelyNode(n); hit {
				return n.Key, id, true
			}
			return "", "", false
		default: // classExternalHTTP / classUnknown
			return "", "", false
		}
	}

	processEdges := func(edges []TopologyEdge) {
		for _, e := range edges {
			if e.From == e.To {
				droppedSelfLoop++
				continue
			}
			fromNode, fromOK := nodes[e.From]
			toNode, toOK := nodes[e.To]
			if !fromOK || !toOK {
				// 端点身份连节点表里都查不到（响应本身不自洽），无法判定分类，
				// 保守丢弃：不产生悬空边。
				continue
			}
			if classify(fromNode) == classExternalHTTP || classify(toNode) == classExternalHTTP {
				droppedExternalHTTP++
				continue
			}

			fromRef, fromEntityID, fromOK2 := resolveEndpoint(fromNode)
			toRef, toEntityID, toOK2 := resolveEndpoint(toNode)
			if !fromOK2 || !toOK2 {
				// 至少一端是未对齐的 RELY_NODE（或未知分类），已记入 Unresolved，
				// 边本身不产生——没有稳定身份就不能落成一条依赖边。
				continue
			}

			key := [2]string{fromRef, toRef}
			stale := e.TotalCount == 0
			if existing, ok := edgeAgg[key]; ok {
				// 同一 (from,to) 在多个来源视图重复出现（testdata 实测存在，见
				// README §3 的交叉校验样本）：只要任一来源观测到流量就不算 stale。
				if !stale {
					existing.Stale = false
				}
				continue
			}
			edgeAgg[key] = &Edge{FromRef: fromRef, ToRef: toRef, FromEntityID: fromEntityID, ToEntityID: toEntityID, Stale: stale}
			edgeOrder = append(edgeOrder, key)
		}
	}

	processEdges(in.Edges)
	for _, s := range svc {
		processEdges(s.Edges)
	}

	// 输出排序保证纯函数的确定性（同输入重放结果一致），不依赖 map 迭代顺序。
	sort.Slice(edgeOrder, func(i, j int) bool {
		if edgeOrder[i][0] != edgeOrder[j][0] {
			return edgeOrder[i][0] < edgeOrder[j][0]
		}
		return edgeOrder[i][1] < edgeOrder[j][1]
	})
	edgesOut := make([]Edge, 0, len(edgeOrder))
	for _, k := range edgeOrder {
		edgesOut = append(edgesOut, *edgeAgg[k])
	}

	sort.Strings(unresolvedOrder)
	unresolvedOut := make([]UnresolvedIdentity, 0, len(unresolvedOrder))
	for _, v := range unresolvedOrder {
		unresolvedOut = append(unresolvedOut, *unresolvedByValue[v])
	}

	return ImportResult{
		Edges:               edgesOut,
		Unresolved:          unresolvedOut,
		ResolvedApps:        resolvedApps,
		DroppedSelfLoop:     droppedSelfLoop,
		DroppedExternalHTTP: droppedExternalHTTP,
	}
}
