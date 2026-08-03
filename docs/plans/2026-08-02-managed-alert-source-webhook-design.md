# Managed Alert Source Webhook 设计

## 目标与边界

SRE Control 为每个入站 Alert Source 分发一条可直接配置到 Alertmanager receiver 的 URL：

```text
https://sre-control.example/ingest/v1/hook/{hook_uuid}
```

`source_id` 是平台内部稳定身份，用于 Alert occurrence、Mapping、Fault 证据与同步水位；
`hook_uuid` 是可轮换的 URL bearer credential。两者必须分离：轮换接入凭据不能切断历史
Alert 与来源的关联，知道 source 主键也不能获得入站写权限。

飞书群自定义机器人 URL 属于出站通知目标，只能由通知渠道在发送阶段读取。它与入站
Alert Source hook 是两个独立信任域，不进入同一字段、响应或日志。

## 凭据模型与请求流程

创建 Source 时使用 `crypto/rand` 生成 RFC 4122 UUID v4；API 只在创建响应显示一次
`hook_id` 与 `webhook_path`，数据库以 `integrations.hook_id_hash` 保存
`hex(sha256(hook_uuid))`。唯一索引保证一个 hook 只映射一个 Source。

入站请求先校验规范 UUID v4，再计算哈希反查 Integration。未知、非法、禁用和已轮换
的旧 hook 对外统一返回 401，避免枚举；日志不记录 hook 明文。成功反查后，adapter、
批量校验、1 MiB 限制、事务入队和 Alertmanager webhook 水位更新完全复用现有链路，
事件最终仍写入稳定 `source_id`。

轮换 API 在单一事务内确认 Source 类型并替换 hook ID hash。事务提交后旧 URL 立即
失效，新 URL 只在该响应显示一次。禁用 Source 也会立即拒绝入站请求。系统不提供
`/ingest/{source_id}`、Authorization 或 X-Ingest-Token 兼容入口。

## 管理交互

控制中心登记 Alertmanager 后弹出一次性接入地址和带 `send_resolved: true` 的 receiver
片段。Source 列表不回显凭据，只展示最近 webhook 水位、API 对账健康和启用状态；管理员
可显式轮换，界面必须提示旧 receiver 会立即失效。编辑普通配置不会隐式轮换 URL。

## 验收

- 创建响应返回规范 UUID v4 与 `/ingest/v1/hook/{uuid}`，后续 GET/List 不泄露；
- 不带 Header 的 managed hook 能识别正确 source_id 并返回 202；
- 非法、未知、禁用和旧 hook 均返回 401 且不入队；
- 轮换后 source_id 不变、旧 URL 失效、新 URL 可接收 Alertmanager firing/resolved；
- 重复 webhook 仍由 occurrence identity 幂等收敛；
- 控制中心可创建、编辑、禁用、轮换、删除和查看 Source 水位；
- 旧 `/ingest/{source_id}` 路由返回 404，入站 hook 与飞书出站 URL 不发生字段或日志串用。
