// Package ccloud 是 ccloud 平台拓扑/健康度接口的只读客户端与纯映射逻辑。
//
// 包边界（不变量 15/23）：本文件只做 HTTP 请求与反序列化，不 import store，
// 也不做任何身份判定或落库——契约坑（key 方向不可信、serviceType 带前导空格、
// health 可空且 0.0 合法等）全部原样透传给调用方，由 importer.go 统一处理。
// 全部契约事实与证据见 internal/ccloud/testdata/README.md。
package ccloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// maxResponseBodyBytes 与 internal/alertsource/client.go 的口径一致：拒绝异常大的
// 响应体，避免单次请求把内存占满。
const maxResponseBodyBytes = 16 << 20

// defaultTimeout 是单次 HTTP 请求的超时时间；调用方可通过 NewClient 注入的
// http.Client 覆盖。
const defaultTimeout = 10 * time.Second

// NodeMetadata 只保留身份判定用得到的字段。CCLOUD_APP 节点的 appId 恒等于 node.key
// （README §7），RELY_NODE/EXTERNAL_HTTP_SERVER 的 metadata 为 null；APISIX/WAF 的
// metadata 含 clusterId/namespace 等业务字段，这里不解析（分类不依赖它们）。
type NodeMetadata struct {
	Type     string `json:"type"`
	NodeType string `json:"nodeType"`
	AppID    string `json:"appId"`
}

// TopologyNode 对应响应 data.nodes 的一个元素。字段选取原则：只留分类判据与展示
// 用途，coreMetrics 一律不解析（两视图量纲/大小写/顺序都不一致，见 README §9）。
type TopologyNode struct {
	Key         string        `json:"key"`         // 唯一稳定标识，恒等于 metadata.appId（CCLOUD_APP 场景）
	ServiceName string        `json:"serviceName"` // 仅展示：不唯一、可含全角、服务拓扑视图里可能退化为 key
	ServiceType string        `json:"serviceType"` // 有前导空格（如 " POSTGRESQL"），使用前必须 TrimSpace
	NodeType    string        `json:"nodeType"`    // 分类判据；EXTERNAL_HTTP_SERVER 节点此处为空
	SLAStandard float64       `json:"slaStandard"`
	Health      *float64      `json:"health"` // 可空；0.0 是合法测量值，不可与 null 混同（README §8）
	Metadata    *NodeMetadata `json:"metadata"`
}

// TrimmedServiceType 返回去除前导/尾随空白后的 serviceType，节点分类判据必须
// 统一走这个方法而不是直接用 ServiceType 字段（README §5）。
func (n TopologyNode) TrimmedServiceType() string {
	return strings.TrimSpace(n.ServiceType)
}

// TopologyEdge 对应响应 data.edges 的一个元素。只信 From/To，edge.key 与
// sourceInfo/targetInfo 里的名字一律不解析——两个视图的 key 方向相反（README §2），
// serviceName 在服务拓扑视图里退化为 key（README §6），都不可信。
type TopologyEdge struct {
	From       string `json:"from"` // 调用发起方（consumer）
	To         string `json:"to"`   // 被调方（provider）
	TotalCount int    `json:"totalCount"`
}

// topologyData 是响应体 data 字段的形状，两个接口共用。
type topologyData struct {
	Nodes []TopologyNode `json:"nodes"`
	Edges []TopologyEdge `json:"edges"`
}

// topologyEnvelope 是拓扑接口的外层信封。message 两个视图字段不一致（全链路多出
// trace 相关字段），且可能为 null（服务拓扑视图），因此用 *string 而不是 string，
// 且不依赖 message 做成功判据——判据统一是 HTTP 状态码 + code==0。
type topologyEnvelope struct {
	Code    int          `json:"code"`
	Message *string      `json:"message"`
	Data    topologyData `json:"data"`
}

// ProjectTopology 是 FetchFullLinkTopology 的返回值：项目级全链路视图。
type ProjectTopology struct {
	ProjectID string
	Nodes     []TopologyNode
	Edges     []TopologyEdge
}

// ServiceTopology 是 FetchServiceTopology 的返回值：单应用的服务拓扑视图，含
// 全链路视图拿不到的 RELY_NODE 基础设施依赖边（README §1）。
type ServiceTopology struct {
	AppID string
	Nodes []TopologyNode
	Edges []TopologyEdge
}

// AppHealth 是 FetchAppHealth 的返回值元素，只取健康度判定必需的两个字段。
type AppHealth struct {
	AppID       string
	Health      *float64 // 可空；0.0 合法
	SLAStandard float64
}

// healthEnvelope 对应健康度接口的响应形状：一个应用一条记录。
type healthEnvelope struct {
	Code    int          `json:"code"`
	Message *string      `json:"message"`
	Data    []healthItem `json:"data"`
}

type healthItem struct {
	AppID       string   `json:"appId"`
	Health      *float64 `json:"health"`
	SLAStandard float64  `json:"slaStandard"`
}

// Client 是 ccloud API 的 HTTP 客户端，只读，不持有任何数据库依赖。
type Client struct {
	baseURL    string
	tokenEnv   string
	httpClient *http.Client
}

// NewClient 构造客户端。baseURL 为空时后续 Fetch 调用会报错（由调用方在装配阶段
// 先做 config.CCloudConfig.Validate() 校验，这里不重复校验配置合法性，只校验用得到
// 的字段）。tokenEnv 是存放 token 的环境变量名（不是 token 本身），沿用
// internal/alertsource/client.go 的 BearerTokenEnv 口径；为空表示不带鉴权头。
func NewClient(baseURL, tokenEnv string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), tokenEnv: tokenEnv, httpClient: httpClient}
}

// FetchFullLinkTopology 拉取项目级全链路拓扑视图（含 APISIX/WAF，不含 RELY_NODE，
// README §1）。
func (c *Client) FetchFullLinkTopology(ctx context.Context, projectID string) (ProjectTopology, error) {
	u, err := c.buildURL("/openapi/v1/topology/fulllink", url.Values{"projectId": {projectID}})
	if err != nil {
		return ProjectTopology{}, err
	}
	env, err := doGet[topologyEnvelope](ctx, c, u)
	if err != nil {
		return ProjectTopology{}, fmt.Errorf("ccloud: 拉取全链路拓扑失败（projectId=%s）: %w", projectID, err)
	}
	return ProjectTopology{ProjectID: projectID, Nodes: env.Data.Nodes, Edges: env.Data.Edges}, nil
}

// FetchServiceTopology 拉取单应用的服务拓扑视图（含 RELY_NODE/EXTERNAL_HTTP_SERVER，
// 不含 APISIX/WAF，且全部 12 条边都围绕该 appId，README §1）。
func (c *Client) FetchServiceTopology(ctx context.Context, appID string) (ServiceTopology, error) {
	u, err := c.buildURL("/openapi/v1/topology/service", url.Values{"appId": {appID}})
	if err != nil {
		return ServiceTopology{}, err
	}
	env, err := doGet[topologyEnvelope](ctx, c, u)
	if err != nil {
		return ServiceTopology{}, fmt.Errorf("ccloud: 拉取服务拓扑失败（appId=%s）: %w", appID, err)
	}
	return ServiceTopology{AppID: appID, Nodes: env.Data.Nodes, Edges: env.Data.Edges}, nil
}

// FetchAppHealth 批量拉取应用健康度，用于 P4 健康证据（本任务只交付 Fetch 本身，
// 不实现调用方）。appIDs 为空时直接返回空结果，不发请求。
func (c *Client) FetchAppHealth(ctx context.Context, appIDs []string) ([]AppHealth, error) {
	if len(appIDs) == 0 {
		return nil, nil
	}
	u, err := c.buildURL("/openapi/v1/health", url.Values{"appIds": {strings.Join(appIDs, ",")}})
	if err != nil {
		return nil, err
	}
	env, err := doGet[healthEnvelope](ctx, c, u)
	if err != nil {
		return nil, fmt.Errorf("ccloud: 拉取应用健康度失败（appIds=%d 个）: %w", len(appIDs), err)
	}
	out := make([]AppHealth, 0, len(env.Data))
	for _, item := range env.Data {
		out = append(out, AppHealth{AppID: item.AppID, Health: item.Health, SLAStandard: item.SLAStandard})
	}
	return out, nil
}

func (c *Client) buildURL(path string, query url.Values) (string, error) {
	if c.baseURL == "" {
		return "", fmt.Errorf("ccloud: base_url 未配置")
	}
	u, err := url.Parse(c.baseURL + path)
	if err != nil {
		return "", fmt.Errorf("ccloud: 构造请求地址失败: %w", err)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// doGet 发起一次 GET 请求并解析为 T。区分三类错误：ctx 超时/取消（%w 包住
// context.DeadlineExceeded/Canceled，调用方可用 errors.Is 判断）、HTTP 层错误
// （连接失败、非 2xx）、业务错误（code != 0）。
func doGet[T any](ctx context.Context, c *Client, rawURL string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return zero, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.tokenEnv != "" {
		token := os.Getenv(c.tokenEnv)
		if token == "" {
			return zero, fmt.Errorf("凭据环境变量 %s 未设置", c.tokenEnv)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// ctx 超时/取消也从这里返回（http.Client 会把 ctx.Err() 包进这个 error），
		// 用 %w 保留可供 errors.Is(err, context.DeadlineExceeded) 判断的链路。
		return zero, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
	if err != nil {
		return zero, fmt.Errorf("读取响应失败: %w", err)
	}
	if len(body) > maxResponseBodyBytes {
		return zero, fmt.Errorf("响应超过 %d 字节", maxResponseBodyBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return zero, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}

	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return zero, fmt.Errorf("解析响应失败: %w", err)
	}
	// code 恒为 0 是成功判据；message 不参与判断（README §12：服务拓扑视图里是 null）。
	if v, ok := any(out).(interface{ codeValue() int }); ok && v.codeValue() != 0 {
		return zero, fmt.Errorf("业务错误 code=%d", v.codeValue())
	}
	return out, nil
}

func (e topologyEnvelope) codeValue() int { return e.Code }
func (e healthEnvelope) codeValue() int   { return e.Code }
