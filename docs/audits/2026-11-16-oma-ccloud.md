# OMA CCloud L4 部署包验收记录

日期：2026-11-16。范围：Issue #27，本地固定只读代理、官方 CLI 离线运行包与现有 Go 日报的追加部署材料。没有修改线上 OMA、IAC 仓库或主 server 入口；线上应用与验收由主流程执行。

## 已验证事实

- 官方安装包 `@cvte/ccloud-cli` 2.2.1，Node>=18.15，env `CCLOUD_API_BASE_URL` 优先于 config apiBaseUrl，`CCLOUD_API_KEY` 优先于 credentials apiKey。只复制 package.json/dist/node_modules 的文件，不打包用户凭据、配置或 native addon。
- 离线包 2,618,568 字节/3447文件，SHA-256 `df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87`。内含逐文件清单并在安装时校验。当前包未包含 Node；主流程提供的安全原生探测事件证实已有 Node v24.18.0/x86_64，本机安装验证使用 Node v22.23.0。
- 本机凭据只安全投影字段与认证状态；GET auth/me 显示 admin=true，currentKey 没有可证明只读的 scope/role 字段。因此现有 key 只驻留 SRE API，不授权 OMA bash 直接持有。安全 expiresAt 元数据为 1805417609000（2027-03-19T00:53:29Z）。没有打印或复制 key 到材料。
- 现有日报资源为 config RO memory、state RW memory、iacctl-go file，现有 OBS_AUTH Vault 与工作日08:30 cron。安全 GET 投影在 [evidence](evidence/2026-11-16-oma-ccloud/deployment.json)；渲染程序保留这些及额外已有资源、metadata、初始事件、预算。没有读取私有完整部署备份。

## 实际验证命令与结果

在 `/private/tmp/alert-oma-ccloud`：

```text
IAC_CCLOUD_CLI_PATH=/tmp/oma-ccloud/ccloud-cli/dist/ccloud-cli.js GOCACHE=/private/tmp/alert-iac-gocache go test -race ./internal/iacccloudproxy -count=1 -v
PASS 3.317s，5项：只读/范围/凭据隔离，坏观测与体积，deadline/并发，官方CLI冷缓存三GET链，上游重定向不能泄漏凭据。
GOCACHE=/private/tmp/alert-iac-gocache go vet ./internal/iacccloudproxy
PASS，退出0。
python3 deploy/oma-ccloud/scripts/test_package.py -v
PASS，3项：原资源/Vault/cron/预算保留，guard拒绝越权与无token，篡改包拒绝。
python3 deploy/oma-ccloud/scripts/install_runtime.py --archive /private/tmp/ccloud-cli-2.2.1-oma.tgz --sha256 df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87 --guard deploy/oma-ccloud/ccloud-cli
连续两次PASS installed=true/file_hashes_verified=true。
python3 deploy/oma-ccloud/scripts/l4_validate.py
无代理token下退出0、status=unknown/cli_exit_code=2/desired_replicas=null，没有发网络查询。
```

冷缓存测试运行真实 Node CLI，隔离 HOME，通过 httptest proxy 到 mock CCloud 上游；它不能代替线上 CCloud 可达与真实 pod schema 证据。官方 CLI 确实在 pods GET 发送2字节 `{}`；代理仅为此固定路由接受精确空对象，其他 body、未知query、appVersion、其他身份与所有mutation拒绝。缺失字段不补零，网络失败不当正常。

## 待线上验收

主流程完成 API 挂线与 Secret、代理窄令牌 Vault、env host白名单追加、旧 Go Agent 新版本、deployment追加资源与 GET readback。随后用 `l4_validation_only` 真实原生工具核对 source、数量/Ready/重启及最终JSON一致，同时回查没有飞书和ledger/memory副作用；正常日报能力仍需原模式实测。当前 desired 副本不在该接口范围内，始终 unknown。未执行固定E2E或写任何线上资源。

部署步骤与真实文件名详见 [operator README](../../deploy/oma-ccloud/README.md)。上传 File ID 有有效期，由操作员作为render参数输入，不写死在源码模板。
