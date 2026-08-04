SHELL := /bin/sh

APP_NAME ?= sre-control
VERSION ?= $(shell sed -n '1p' VERSION)
GIT_COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || printf unknown)
IMAGE_REGISTRY ?=
IMAGE_PREFIX := $(if $(IMAGE_REGISTRY),$(IMAGE_REGISTRY)/,)
API_IMAGE ?= $(IMAGE_PREFIX)$(APP_NAME)-api:$(VERSION)
WEB_IMAGE ?= $(IMAGE_PREFIX)$(APP_NAME)-web:$(VERSION)
MIGRATE_IMAGE ?= $(IMAGE_PREFIX)$(APP_NAME)-migrate:$(VERSION)
BINARY := bin/alert-system
SEED_BINARY := bin/seed-admin
K8S_NAMESPACE ?= sre-control
K8S_OVERLAY ?= deploy/k8s/overlays/local
DOCKER_BUILD_PROGRESS ?= plain
DOCKER_BUILDX_BUILDER ?=
DOCKER_LOCAL_PLATFORM ?=
DOCKER_PUSH_PLATFORMS ?= linux/amd64
GO_BASE_IMAGE ?= registry.gz.cvte.cn/library/golang:1.25-alpine
ALPINE_BASE_IMAGE ?= registry.gz.cvte.cn/library/alpine:3.22
NODE_BASE_IMAGE ?= registry.gz.cvte.cn/library/node:22-alpine
GO111MODULE ?= on
GOPROXY ?= https://goproxy.cn,direct
GOSUMDB ?= sum.golang.google.cn
NPM_REGISTRY ?= https://registry.npmmirror.com
BUILDX_BUILDER_ARG := $(if $(DOCKER_BUILDX_BUILDER),--builder $(DOCKER_BUILDX_BUILDER),)
BUILDX_LOCAL_PLATFORM_ARG := $(if $(DOCKER_LOCAL_PLATFORM),--platform $(DOCKER_LOCAL_PLATFORM),)
DOCKER_BASE_IMAGE_ARGS := --build-arg GO_BASE_IMAGE=$(GO_BASE_IMAGE) --build-arg ALPINE_BASE_IMAGE=$(ALPINE_BASE_IMAGE) --build-arg NODE_BASE_IMAGE=$(NODE_BASE_IMAGE)
DOCKER_GO_MODULE_ARGS := --build-arg GO111MODULE=$(GO111MODULE) --build-arg GOPROXY=$(GOPROXY) --build-arg GOSUMDB=$(GOSUMDB)
DOCKER_WEB_ARGS := --build-arg NPM_REGISTRY=$(NPM_REGISTRY)
DOCKER_METADATA_ARGS := --build-arg VERSION=$(VERSION) --build-arg VCS_REF=$(GIT_COMMIT)
DOCKER_BUILD_ARGS := $(DOCKER_BASE_IMAGE_ARGS) $(DOCKER_GO_MODULE_ARGS) $(DOCKER_WEB_ARGS) $(DOCKER_METADATA_ARGS)

.DEFAULT_GOAL := help

.PHONY: help version build build-tools run seed-admin fmt-check vet test test-ci \
	backend-ci web-install web-build web-lint frontend-ci ci release-check tidy \
	web-audit docker-check docker-build docker-build-api docker-build-web docker-build-migrate docker-smoke \
	docker-push docker-push-check docker-push-api docker-push-web docker-push-migrate \
	k8s-render k8s-validate k8s-deploy k8s-wait k8s-port-forward k8s-bootstrap-admin \
	kind-load

help: ## 显示可用目标
	@awk 'BEGIN {FS = ":.*## "; printf "SRE Control %s\n\n", "$(VERSION)"} /^[a-zA-Z0-9_.-]+:.*## / {printf "  %-22s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

version: ## 输出版本和镜像名
	@printf 'version=%s\napi=%s\nweb=%s\nmigrate=%s\ngo_base=%s\nalpine_base=%s\nnode_base=%s\ngo111module=%s\ngoproxy=%s\ngosumdb=%s\nnpm_registry=%s\n' "$(VERSION)" "$(API_IMAGE)" "$(WEB_IMAGE)" "$(MIGRATE_IMAGE)" "$(GO_BASE_IMAGE)" "$(ALPINE_BASE_IMAGE)" "$(NODE_BASE_IMAGE)" "$(GO111MODULE)" "$(GOPROXY)" "$(GOSUMDB)" "$(NPM_REGISTRY)"

build: ## 构建服务端二进制
	@mkdir -p bin
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BINARY) ./cmd/server

build-tools: ## 构建管理员引导工具
	@mkdir -p bin
	go build -trimpath -o $(SEED_BINARY) ./cmd/seed-admin

run: ## 启动本地 API
	go run -ldflags "-X main.version=$(VERSION)" ./cmd/server

seed-admin: build-tools ## 使用 ADMIN_PASSWORD 引导本地管理员
	@test -n "$(ADMIN_PASSWORD)" || { printf '%s\n' 'ADMIN_PASSWORD 不能为空'; exit 2; }
	$(SEED_BINARY) -password "$(ADMIN_PASSWORD)" -username "$(or $(ADMIN_USERNAME),admin)" -team "$(or $(ADMIN_TEAM),default)"

fmt-check: ## 检查 Go 格式，不修改文件
	@files="$$(find cmd internal test -type f -name '*.go' -print)"; unformatted="$$(gofmt -l $$files)"; test -z "$$unformatted" || { printf '%s\n' "需要 gofmt 的文件:" "$$unformatted"; exit 1; }

vet: ## 运行 go vet
	go vet ./...

test: ## 运行快速 Go 测试
	go test ./...

test-ci: ## 运行带真实 PostgreSQL 的完整 Go 测试
	go test -tags=integration ./... -race -count=1

backend-ci: fmt-check ## 复现后端 CI
	go build ./...
	go vet ./...
	go test -tags=integration ./... -race -count=1

web-install: ## 使用 lockfile 安装前端依赖
	cd web && npm ci

web-build: ## 构建生产前端
	cd web && npm run build

web-lint: ## 检查前端代码
	cd web && npm run lint

frontend-ci: web-install web-build web-lint ## 复现前端 CI

ci: backend-ci frontend-ci ## 复现 GitHub Actions 本地 CI

web-audit: ## 审计生产依赖，critical 漏洞阻断发布
	cd web && npm audit --omit=dev --audit-level=critical

release-check: ci web-audit docker-check docker-build docker-smoke k8s-validate ## 发布前完整检查

tidy: ## 整理 Go modules
	go mod tidy

docker-check: ## 解析 Dockerfile 并列出全部构建 target（无需拉取基础镜像）
	docker buildx build $(BUILDX_BUILDER_ARG) $(DOCKER_BUILD_ARGS) --call=targets --progress=$(DOCKER_BUILD_PROGRESS) .

docker-build: docker-build-api docker-build-web docker-build-migrate ## 构建全部本地镜像

docker-build-api: ## 构建 API 镜像
	docker buildx build $(BUILDX_BUILDER_ARG) $(BUILDX_LOCAL_PLATFORM_ARG) $(DOCKER_BUILD_ARGS) --load --progress=$(DOCKER_BUILD_PROGRESS) --target api -t $(API_IMAGE) .

docker-build-web: ## 构建 Web 镜像
	docker buildx build $(BUILDX_BUILDER_ARG) $(BUILDX_LOCAL_PLATFORM_ARG) $(DOCKER_BUILD_ARGS) --load --progress=$(DOCKER_BUILD_PROGRESS) --target web -t $(WEB_IMAGE) .

docker-build-migrate: ## 构建数据库迁移镜像
	docker buildx build $(BUILDX_BUILDER_ARG) $(BUILDX_LOCAL_PLATFORM_ARG) $(DOCKER_BUILD_ARGS) --load --progress=$(DOCKER_BUILD_PROGRESS) --target migrate -t $(MIGRATE_IMAGE) .

docker-smoke: ## 验证镜像入口、非 root 用户和 nginx 配置
	@test "$$(docker image inspect $(API_IMAGE) --format '{{.Config.User}}')" = "10001:10001"
	docker run --rm --entrypoint /bin/sh $(API_IMAGE) -ec 'test "$$(id -u)" = 10001 && test -x /app/alert-system && test -x /app/seed-admin'
	@test "$$(docker image inspect $(WEB_IMAGE) --format '{{.Config.User}}')" = "101:101"
	docker run --rm -e API_BASE_URL=/api $(WEB_IMAGE) --check
	docker run --rm -e API_BASE_URL=https://sre-control.example.com/api $(WEB_IMAGE) --check
	@if docker run --rm $(WEB_IMAGE) --check >/dev/null 2>&1; then printf '%s\n' 'Web 镜像未拒绝缺失的 API_BASE_URL'; exit 1; fi
	@if docker run --rm -e API_BASE_URL='sre-control.example.com/api' $(WEB_IMAGE) --check >/dev/null 2>&1; then printf '%s\n' 'Web 镜像未拒绝非法的 API_BASE_URL'; exit 1; fi
	@test "$$(docker image inspect $(MIGRATE_IMAGE) --format '{{.Config.User}}')" = "65532:65532"
	docker run --rm $(MIGRATE_IMAGE) -version

docker-push-check:
	@test -n "$(IMAGE_REGISTRY)" || { printf '%s\n' 'IMAGE_REGISTRY 不能为空'; exit 2; }

docker-push: docker-push-api docker-push-web docker-push-migrate ## 使用 buildx 构建并推送镜像

docker-push-api: docker-push-check ## 构建并推送 API 镜像
	docker buildx build $(BUILDX_BUILDER_ARG) --platform $(DOCKER_PUSH_PLATFORMS) $(DOCKER_BUILD_ARGS) --push --progress=$(DOCKER_BUILD_PROGRESS) --target api -t $(API_IMAGE) .

docker-push-web: docker-push-check ## 构建并推送 Web 镜像
	docker buildx build $(BUILDX_BUILDER_ARG) --platform $(DOCKER_PUSH_PLATFORMS) $(DOCKER_BUILD_ARGS) --push --progress=$(DOCKER_BUILD_PROGRESS) --target web -t $(WEB_IMAGE) .

docker-push-migrate: docker-push-check ## 构建并推送迁移镜像
	docker buildx build $(BUILDX_BUILDER_ARG) --platform $(DOCKER_PUSH_PLATFORMS) $(DOCKER_BUILD_ARGS) --push --progress=$(DOCKER_BUILD_PROGRESS) --target migrate -t $(MIGRATE_IMAGE) .

k8s-render: ## 渲染本地 Kubernetes overlay
	@kubectl kustomize $(K8S_OVERLAY)

k8s-validate: ## 客户端校验 Kubernetes manifests
	kubectl kustomize $(K8S_OVERLAY) | kubectl apply --dry-run=client --validate=false -f - >/dev/null

k8s-deploy: ## 部署本地 overlay 到当前 kubectl context
	kubectl apply -k $(K8S_OVERLAY)

k8s-wait: ## 等待 PostgreSQL 与 SRE Control 就绪
	kubectl -n $(K8S_NAMESPACE) rollout status statefulset/sre-control-postgres --timeout=180s
	kubectl -n $(K8S_NAMESPACE) rollout status deployment/sre-control --timeout=180s

k8s-port-forward: ## 将本地 5173/8080 分别转发到集群 Web/API Service
	kubectl -n $(K8S_NAMESPACE) port-forward service/sre-control 5173:80 8080:8081

k8s-bootstrap-admin: ## 运行一次管理员引导 Job（本地默认 admin/admin123）
	kubectl -n $(K8S_NAMESPACE) delete job sre-control-seed-admin --ignore-not-found
	kubectl apply -f deploy/k8s/jobs/seed-admin.yaml
	kubectl -n $(K8S_NAMESPACE) wait --for=condition=complete job/sre-control-seed-admin --timeout=120s

kind-load: docker-build ## 将本地镜像加载到 kind 集群（KIND_CLUSTER 可覆盖）
	kind load docker-image --name "$(or $(KIND_CLUSTER),kind)" $(API_IMAGE) $(WEB_IMAGE) $(MIGRATE_IMAGE)
