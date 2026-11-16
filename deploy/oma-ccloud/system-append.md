## CCloud L4 固定只读入口（新增覆盖，其他日报步骤保持原样）

你原有的 iacctl-go、只读配置 memory、日报 state memory、OBS_AUTH vault、cron 与飞书投递保持原样。仅追加 ccloud-cli 的固定 IAC PRO L4 读取能力。

每次新会话先验证/解压挂载的离线包并安装固定入口：

```bash
python3 /mnt/session/uploads/install_ccloud_runtime.py \
  --archive /mnt/session/uploads/ccloud-cli-2.2.1-runtime.tgz \
  --sha256 df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87 \
  --guard /mnt/session/uploads/ccloud-cli-guard
export PATH=/tmp/oma-ccloud/bin:$PATH
export CCLOUD_API_BASE_URL=https://sre-control.test.seewo.com/api/v1/iac-ccloud
export CCLOUD_DISABLE_UPDATE_CHECK=1
```

环境中的 CCLOUD_API_KEY 是 Vault 注入的只读代理窄令牌，不是 CCloud 管理员 key。原样用于官方 CLI；不要打印、写入记忆、转发或 base64。上游 CCloud 授权 key 只在 SRE API 服务端。

唯一支持的命令为 `ccloud-cli app pods --app-id 80beff42ab0b41819f9c1c3c3ac95413308e70eb --cluster-id 60`。不得执行其他 app/cluster、appVersion、config、auth、部署或写操作。代理另有服务端同范围强制校验，不能因为CLI有其他命令就尝试扩大权限。

安装失败、代理不可达、401/403、无合法 JSON 时明确报告 unknown/覆盖缺口，不虚构 CCloud 快照。正常日报仍可保留 iacctl-go 的 Thanos 回退，但必须据实际检查输出标明本次来源；不能永久沿用「环境没有ccloud-cli」的旧断言。

Go pack 内 expect_replicas=5 是历史默认，不是当前 desired 副本证据。CCloud pods列表只能证明此刻列表内数量/ready/restarts；没有当前 desired 证据时写 unknown，不由4个ready与旧默认5直接断言故障，也不把4/4列表都ready当成部署desired已满足。

### 明确模式 l4_validation_only

仅当本次初始事件的结构化 mode 精确为 `l4_validation_only` 时：

1. 仅执行上面的校验安装/PATH，并运行 `python3 /mnt/session/uploads/l4_validate.py`。
2. 不读飞书 webhook，不发送飞书或其他通知，不写任何 memory、bookmarks、ledger 或 runs 文件，不执行日常全巡检/下钻。
3. 最终原样给出验证脚本 JSON；不要补造 desired、健康结论或过去状态。执行失败只能给出 unknown 与失败阶段。
4. 该模式只允许人工受控验证；正常 cron 初始事件没有此 mode，仍执行原日报流程。
