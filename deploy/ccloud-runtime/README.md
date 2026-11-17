# CCloud 官方 CLI 的离线镜像输入

API 与可选 OMA worker 需要 Node 和 `@cvte/ccloud-cli` 2.2.1。运行期不执行 npm install、不下载 CLI，也不把上游 CCloud key 放进镜像。CLI 只通过固定 broker 取得代理窄令牌和固定 URL；完整命令集可用不意味着 broker 获得这些权限。

## 可信输入与运行路径

| 项目 | 固定值 |
| --- | --- |
| 官方包 | @cvte/ccloud-cli 2.2.1 |
| Node 引擎最低要求 | >=18.15 |
| 审计归档 SHA-256 | df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87 |
| 原始 BUNDLE.json 字节 SHA-256 | 036011de6bb8af591eb38b1119d54f5956d9ba44dd46731df523ee31e2690c28 |
| NodeBinary | /usr/local/bin/node |
| PackageDir | /opt/ccloud-cli |
| CLI JS 入口 | /opt/ccloud-cli/dist/ccloud-cli.js |

归档是已审计的官方安装包文件快照，不是可重新打包后默认信任的任意 npm tarball。当前该归档仅存在于私有离线材料；必须将原始字节归档到受控 artifact 存储并保留 digest、包来源和审计记录。仅 clone 仓库不足以构建 API；需要先恢复这个输入。#27 的 pack_runtime.py 使用 tarfile/gzip，归档头 mtime 会随时间变化，所以重新 npm 安装或重新打包不能保证得到相同 archive SHA。这里“可复现”是固定已审计原归档、固定内容清单及固定镜像来源的构建输入方案，不是对任意重新打包给出相同压缩字节的承诺。后续更新应重新审计 manifest/逐文件hash并显式更新可信常量，不能通过覆盖SHA参数跳过校验。其 BUNDLE.json 包含 `package/version/node_requirement/credential_files_included=false/native_addons=false/files[{path,sha256}]`，逐个登记3447个普通文件；manifest 本身不自引用。Go runtime 应固定独立的上述 manifest 内容 hash，不能复用 iacctl-go 的 hash。构建器原样保留 BUNDLE.json 字节，校验归档、manifest、包身份、所有文件 hash、无遗漏/额外文件/软链接/native addon，再以 root 所有、普通文件0444落入运行镜像。不要把整个 node_modules 或归档提交 Git。

`CCLOUD_NODE_BASE_IMAGE` 与 Web 的 `NODE_BASE_IMAGE` 分开。当前已验证并固定为 `registry.gz.cvte.cn/library/node:22-alpine@sha256:dd31daf8a971114cd1084bb815d45a81cbc58081496149de97bd8ba814a8ab4e`：源镜像是 Node22.22.2/Alpine3.23.4。API/worker 最终仍为 Alpine3.22，补 `libstdc++/libgcc`，保留 CA、tzdata 与 UID10001。最小 builder probe 已实际将此 Node 复制到 Alpine3.22.2 并执行成功，且用最终环境验证官方 CLI 冷缓存；这是实测ABI兼容，不宣称源镜像是3.22。Makefile显式注入该内网固定digest，允许操作员覆盖为另一个经过同等验证的内网副本；不新增隐式DockerHub访问。Web使用原先的 Node 配置。

## 准备与标准构建

从安全制品存储取得已审计归档，文件名不影响校验。上传资源 ID 或带签名下载 URL 不能进入源码模板；制品恢复/传输由操作员负责。以下输入只包含文件路径和镜像引用，不含 token：

```bash
make ccloud-runtime-prepare \
  CCLOUD_RUNTIME_ARCHIVE=/secure/artifacts/ccloud-cli-2.2.1-oma.tgz \
  CCLOUD_RUNTIME_CONTEXT=/secure/build-input/ccloud-runtime
make docker-build-api \
  CCLOUD_RUNTIME_CONTEXT=/secure/build-input/ccloud-runtime \
  CCLOUD_NODE_BASE_IMAGE=internal-registry.example/node:22-alpine3.22@sha256:REVIEWED_DIGEST
```

context 必须是新目录，且只含 `runtime.tgz`；已有目录不自动覆盖。标准 `docker-build-api`、`docker-push-api` 会先检查 Node 镜像参数与归档，缺失时在调用 Docker 前停止并提示准备命令。默认 context 为仓库 `bin/ccloud-runtime`（bin 被现有 .dockerignore 排除）；named context 单独提供，不把该二进制混入普通源码 context。Web/migrate 不读取此输入，现有 Go/Web CI 无镜像构建步骤，不需归档即可运行。

直接使用 buildx 时必须提供：

```bash
docker buildx build --target api \
  --build-context ccloud_runtime=/secure/build-input/ccloud-runtime \
  --build-arg CCLOUD_NODE_BASE_IMAGE=internal-registry.example/node:22-alpine3.22@sha256:REVIEWED_DIGEST .
```

可选 worker 的 `make -f deploy/iac-worker/Makefile image` 同样接收这两个 `CCLOUD_*` 参数，还需按原 recipe 显式传 Go/Alpine/proxy 参数。CCloud 托管 Dockerfile 构建入口若不支持 named context，应使用已准备好的本地 BuildKit 发布 recipe 构建镜像后按镜像发布；不能删掉校验或偷偷改成运行期下载。未经恢复的离线输入不支持裸 `docker build .`，请使用上面带 fail-fast 的标准入口。

## 只读根与缓存

官方 CLI 2.2.1 会使用 `os.homedir()/.ccloud/cache` 写元数据，没有可配 cache env。API/worker 的 UID10001 passwd home 均为 `/tmp/sre-control`，不设置运行 HOME env；broker 子进程仍应剥离 HOME、NODE_OPTIONS、OBS/OMA/上游 key，只传代理窄令牌。使用只读 root filesystem 时，必须挂载 UID10001 可写的 `/tmp`（如 mode1777 tmpfs，或配置相应 fsGroup/权限的 emptyDir）。冷缓存时 CLI 会递归创建该 home 下的 cache；不需要构建期写入个人配置。并发执行仍由 broker 有限槽位限制。

## 验证与边界

```bash
CCLOUD_TEST_ARCHIVE=/secure/artifacts/ccloud-cli-2.2.1-oma.tgz \
  python3 -m unittest discover -s deploy/ccloud-runtime -p 'test_*.py' -v
```

没有 `CCLOUD_TEST_ARCHIVE` 时真实包验证会 skip，不能把跳过算通过。本轮实际运行5项测试全通过，包含真实归档逐文件校验、篡改包、非纯净/软链接context拒绝、标准API缺输入先于Docker失败、Web/migrate无context依赖。两份 Dockerfile 的 `--call=targets` 均通过，没有构建/发布线上镜像。

`smoke-cache.mjs` 是容器内合成冷缓存脚本，只请求容器 loopback mock。验收时用 UID10001、只读root、network none、可写/tmp，挂载合成passwd（UID10001 home=/tmp/sre-control）、已验证包到/opt/ccloud-cli和该脚本到/smoke-cache.mjs，然后以 `/usr/bin/env -u HOME /usr/local/bin/node /smoke-cache.mjs` 启动。只输出通过状态，验证真实官方CLI三GET、cache创建、未缓存窄令牌，不执行 `ccloud-cli --version`。

本轮先用已有本地 Linux Node22.23.2 镜像验证 passwd home/无HOME缓存行为；后续按授权使用既有 multiarch-builder 构建最小本地 probe（无业务发布）：内网固定源 Node22.22.2/Alpine3.23.4 → 最终 Alpine3.22.2 + libstdc++/libgcc，`node --version` 成功。该最终 probe 以 UID10001、只读root、network none、可写/tmp和实际passwd home运行真实CLI冷缓存测试通过，且metadata cache不包含合成窄令牌。没有执行CLI版本命令，所有HTTP仅容器内合成mock。

完整API/worker镜像构建与业务canary仍由发布流程执行；最小probe不代替最终镜像验收。依赖缺失必须unknown，不能回退为真实正常。包来源、manifest hash与实际Node版本均应进入provenance，不能以包存在代替现场成功证据。
