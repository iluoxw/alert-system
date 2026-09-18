# ccloud 拓扑接口契约事实（fixture 证据集）

本目录保存 **2026-09-18 由用户提供的两份 ccloud 真实响应**，逐字段原样存储，作为 `internal/ccloud`
适配器的契约基准与测试 fixture。

| 文件 | 视图 | 节点 | 边 | 外层信封 |
|---|---|---|---|---|
| `fulllink_topology.json` | 项目级全链路 | 24 | 77 | `message:"ok"` + `traceId`/`serverTimeCost`/`k8sApiTotalTimeCost`/`k8sApiInvokeTimes`/`serverIp` |
| `service_topology_cplm-boot-prod.json` | 单应用服务拓扑（appId=`4f2b08e91ce2f89281397c4579f8f5f435e69985` CPLM-BOOT-PROD） | 22 | 12 | `message:null`，无 trace 字段 |

两者互相印证，也互相矛盾。下面每条都标注了**证据**，改动适配器前请先核对。

---

## 1. 两个视图的覆盖范围不同（印证 ADR-0006）

- 全链路视图：含 `APISIX`（1）与 `WAF`（1），**不含 `RELY_NODE`**。
- 服务拓扑视图：含 `RELY_NODE`（5）与 `EXTERNAL_HTTP_SERVER`（1），**不含 `APISIX`/`WAF`**；
  12 条边全部围绕被查询的那个 appId。
- ⇒ **基础设施边（App→DB/Redis/Oracle）必须按应用逐个拉取**，全链路视图拿不到。
- 全链路视图没有 `:0`、没有 `RELY_NODE`，所以「未对齐身份」问题只在服务拓扑视图暴露。

## 2. `edge.key` 的方向两个视图相反 —— **key 完全不可信**

- 全链路：**77/77** 条满足 `key == from + "->" + to`
- 服务拓扑：**12/12** 条满足 `key == to + "->" + from`

```
全链路    {"from":"6c66df2c…","to":"034fbe22…","key":"6c66df2c…->034fbe22…"}
服务拓扑  {"from":"89866988…","to":"4f2b08e9…","key":"4f2b08e9…->89866988…"}
```

⇒ **只信 `from`/`to`**，忽略 `key`。（ADR-0006 只说了"服务拓扑视图里 key 与 from/to 相反"，
更精确的表述是"两个视图的 key 方向不一致，因此不可用"。）

## 3. `from`/`to` 语义 = 调用发起方 → 被调方，与我们 `dependency_edges` 约定同向

**决定性证据**：`4f2b08e9…（CPLM-BOOT-PROD）→ 308e2cca…（REDIS）`，`totalCount=55264`，
`requestRate≈30.7/s`。只可能是**应用调用 Redis**，Redis 不会调用应用。故 `from` = consumer，`to` = provider。

同一关系在两个视图里方向一致（`6c66df2c…（CPLM-PDM-PROD）→ 4f2b08e9…（CPLM-BOOT-PROD）`，
两处 `totalCount` 均为 2414），可交叉校验。

⇒ **`from` 直接写 `dependency_edges.from`，`to` 直接写 `to`，不需要翻转。**

⚠️ **由此产生的已知缺口（需产品裁决，见 §10）**：入口链是 `WAF → APISIX → App`，即入口在 `from` 侧。
而爆炸半径是"从故障 provider 沿反向边遍历"，因此 **APISIX/WAF 故障不会被传播到应用**。

## 4. 节点分类判据是 `nodeType`，**为空时回落到 `serviceType`**

| nodeType | 出现次数（全链路/服务拓扑） |
|---|---|
| `CCLOUD_APP` | 22 / 16 |
| `APISIX` | 1 / 0 |
| `WAF` | 1 / 0 |
| `RELY_NODE` | 0 / 5 |
| `""`（空） | 0 / 1 |

唯一的空 `nodeType` 节点：

```json
{"key":"a82b1dd76e5f4f7abed1271449f4ef97","serviceName":"cplmobj.gz.cvte.cn:443",
 "serviceType":"EXTERNAL_HTTP_SERVER","nodeType":"","metadata":null}
```

⇒ 只判 `nodeType` 会**漏掉**这个节点（既无法识别、也无法按规则"忽略 EXTERNAL_HTTP_SERVER"）。
判据必须是：`nodeType` 非空时用它，否则回落 `serviceType`。

## 5. `serviceType` 有前导空格

实际取值集合：`SERVER`、`APISIX`、`WAF`、`EXTERNAL_HTTP_SERVER`、`REDIS`、`ORACLE`、`" POSTGRESQL"`（**带前导空格**）。

⇒ 切换 `serviceType` 前必须 `strings.TrimSpace`。

## 6. `serviceName` 不能作为身份

- **不唯一**：服务拓扑视图内 `lcp-app` 出现 **2 次**，是两个不同应用
  （key `6f158bfe4048489996fdfd6c` sla 0.997；key `f8b2cda666e249d68cb37d65` sla 0.9993）。
- **可能含全角字符**：`PRJ-INFO-PROD（cplm-ipm）`。
- **在服务拓扑视图里退化为 id**：`sourceInfo.serviceName` 直接等于 `key`
  （如 `"89866988f38d424e805fd0e9"`）；全链路视图里才是真实服务名。同一字段两种语义。
- RELY_NODE 的 `serviceName` 是 `host:port`，且可能是 `":0"`。

⇒ 身份只取 `node.key`（= `metadata.appId`），`serviceName` 仅用于展示。

## 7. `metadata.appId` 长度不固定

本项目内同时存在 **24 字符**与 **40 字符**两种 appId（全链路 7/15，服务拓扑 9/7 混合），且
`metadata.appId` 恒等于 `node.key`。

⇒ **禁止按长度或正则校验 appId**，一律当不透明字符串处理。

## 8. `health` 可空，且 `0.0` 是合法值

- 服务拓扑：5 个 RELY_NODE + 1 个 EXTERNAL_HTTP_SERVER 全部 `health: null`
- 全链路：`chang_ting_waf` = `null`；`nginx-prd-it`（APISIX）= **`0.0`**，同时 `slaStandard: 1.0`
  ⇒ 平台当前就认为该入口破线，且这是**真实测量值**，不是缺失

⇒ Go 侧必须是 `*float64`。把 `0` 当"缺失"会误判 APISIX；把缺失当 `0` 会把无数据节点判成破线。

## 9. `coreMetrics` 形状不稳定，不得用于判定

| | 全链路 | 服务拓扑 |
|---|---|---|
| `description` | `"QPS"` / `"平均耗时"` / `"错误率"` | `"qps"` / `"平均耗时"` / `"错误率"` |
| 顺序 | QPS→耗时→错误率 | 错误率→qps→耗时 |
| `unit` | 全部 `""` | `"percentage"` / `"qps"` / `"millisecond"` |

同一节点（`6c66df2c…`）的「错误率」在两视图里分别是 `1.346` 与 `0.0024167`，量纲不一致。

⇒ **只用 `health` vs `slaStandard` 做判定**，`coreMetrics` 一概不解析。

## 10. 需要裁决的两个问题

### 10.1 入口（APISIX/WAF）故障不会传播到应用

见 §3。数据里入口在 `from` 侧，反向遍历只能从 App 找到 APISIX/WAF，反之不行。
即：**APISIX 挂了 → 没有任何应用被标记受影响**。

- 保守解读（当前实现口径）：入口抖动 ≠ 应用 SLO 破线，不外播可避免误报。
- 若要让入口故障外播，需要把 `WAF/APISIX → App` 的边**反向**存储（或额外加一条 provider 方向的边），
  这会让 App 同时是 provider 与 consumer，语义变复杂。

**建议**：保持现状（照抄 from/to），把它记为已知缺口，等有真实入口故障案例后再决定。

### 10.2 `EXTERNAL_HTTP_SERVER` 该忽略还是进待对齐队列

样例中它**没有任何边**（`a82b1dd76e5f4f7abed1271449f4ef97` 不出现在任何 `from`/`to`），
所以按 ADR-0006「忽略」不丢信息。

但它 `serviceName` 是 `cplmobj.gz.cvte.cn:443` —— 与 RELY_NODE 同形的 `host:port`，
语义上是真实的依赖（对象存储）。若将来出现边，忽略就会丢真实边。

**建议**：仍按 ADR 忽略，但在 importer 里留一处显式计数（`droppedExternalHTTP`）并打进同步日志，
一旦非零就提示重新评估。

## 11. `:0` 的身份锚点必须用 `node.key`，不能用 `host:port`

```json
{"key":"308e2cc521d64075b181a08e6059cb5b","serviceName":":0","serviceType":"REDIS","nodeType":"RELY_NODE"}
```

它有**稳定的 key**，且是真实边的目标（`4f2b08e9… → 308e2cca…`，55264 次调用）。

若 `unresolved_identities` 以 `host:port` 作主键值，则**所有报 `:0` 的 Redis 会塌进同一行**互相覆盖
（主键 `(namespace, value)` 只有一行），且人看到 `:0` 无法判断是哪个实例。

⇒ 待对齐队列的 `value` 用 **`node.key`**，`host_port` 与 `service_type` 作为展示列；
人工建别名时同时写 `node.key` 与（合法时的）`host:port` 两个 alias，兼顾稳定性与可读性。

## 12. 其他

- `totalCount == 0` 的边：全链路 **40/77**，服务拓扑 0 —— 占比很高，`stale` 标记是常态而非异常。
- 自环：全链路 2 条（`6c66df2c→6c66df2c` totalCount 18237 **有流量**；`3367c6e2→3367c6e2` totalCount 0）
  ⇒ 丢弃，且注意"自环也可能有流量"，不能靠 totalCount 判断。
- 外层 `message` 类型不稳定（`"ok"` / `null`），且全链路多出 5 个 trace 字段 ⇒ DTO 必须容忍未知字段
  与 `message` 的可空。
- 响应顶部 `code` 恒为 `0`；`message` 不作为成功判据（服务拓扑里是 `null`）。

---

## 复现校验

```bash
cd internal/ccloud/testdata && python3 - <<'PY'
import json
for p, n, e, d in [("service_topology_cplm-boot-prod.json",22,12,"to->from"),
                   ("fulllink_topology.json",24,77,"from->to")]:
    x = json.load(open(p)); nd, ed = x["data"]["nodes"], x["data"]["edges"]
    keys = {k["key"] for k in nd}
    assert (len(nd), len(ed)) == (n, e), (p, len(nd), len(ed))
    assert all(g["from"] in keys and g["to"] in keys for g in ed), p
    f = (lambda g: f'{g["to"]}->{g["from"]}') if d == "to->from" else (lambda g: f'{g["from"]}->{g["to"]}')
    assert all(g["key"] == f(g) for g in ed), p
    print(p, "OK", len(nd), len(ed))
PY
```
