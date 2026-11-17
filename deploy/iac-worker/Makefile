# Run from repository root: make -f deploy/iac-worker/Makefile image
# Internal deployments MUST explicitly pass mirrors/proxies; no implicit external fetch.
IMAGE ?= sre-control-iac-worker:dev
PLATFORM ?= linux/amd64
WORKER_PACKAGE ?= ./cmd/oma-worker
VCS_REF ?= unknown
GO_BASE_IMAGE ?=
CCLOUD_NODE_BASE_IMAGE ?= registry.gz.cvte.cn/library/node:22-alpine@sha256:dd31daf8a971114cd1084bb815d45a81cbc58081496149de97bd8ba814a8ab4e
CCLOUD_RUNTIME_CONTEXT ?= $(CURDIR)/bin/ccloud-runtime
ALPINE_BASE_IMAGE ?=
GOPROXY ?=
GOSUMDB ?=
BUILD_ARGS = --build-context "ccloud_runtime=$(CCLOUD_RUNTIME_CONTEXT)" --build-arg "CCLOUD_NODE_BASE_IMAGE=$(CCLOUD_NODE_BASE_IMAGE)" --build-arg GO_BASE_IMAGE=$(GO_BASE_IMAGE) --build-arg ALPINE_BASE_IMAGE=$(ALPINE_BASE_IMAGE) --build-arg GOPROXY=$(GOPROXY) --build-arg GOSUMDB=$(GOSUMDB) --build-arg WORKER_PACKAGE=$(WORKER_PACKAGE) --build-arg VCS_REF=$(VCS_REF)
.PHONY: check image push
check:
	@test -n "$(CCLOUD_NODE_BASE_IMAGE)" || (echo 'Specify audited CCLOUD_NODE_BASE_IMAGE (internal mirror for internal builds)'; exit 1)
	python3 deploy/ccloud-runtime/prepare_context.py --check --output "$(CCLOUD_RUNTIME_CONTEXT)"
	@test -n "$(GO_BASE_IMAGE)" -a -n "$(ALPINE_BASE_IMAGE)" -a -n "$(GOPROXY)" -a -n "$(GOSUMDB)" || (echo 'Specify internal GO_BASE_IMAGE/ALPINE_BASE_IMAGE/GOPROXY/GOSUMDB'; exit 1)
image: check
	docker buildx build --platform $(PLATFORM) --load -f deploy/docker/Dockerfile.iac-worker $(BUILD_ARGS) --target iac-worker -t $(IMAGE) .
push: check
	docker buildx build --platform $(PLATFORM) --push -f deploy/docker/Dockerfile.iac-worker $(BUILD_ARGS) --target iac-worker -t $(IMAGE) .
