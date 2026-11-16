# Run from repository root: make -f deploy/iac-worker/Makefile image
# Internal deployments MUST explicitly pass mirrors/proxies; no implicit external fetch.
IMAGE ?= sre-control-iac-worker:dev
PLATFORM ?= linux/amd64
WORKER_PACKAGE ?= ./cmd/oma-worker
VCS_REF ?= unknown
GO_BASE_IMAGE ?=
ALPINE_BASE_IMAGE ?=
GOPROXY ?=
GOSUMDB ?=
BUILD_ARGS = --build-arg GO_BASE_IMAGE=$(GO_BASE_IMAGE) --build-arg ALPINE_BASE_IMAGE=$(ALPINE_BASE_IMAGE) --build-arg GOPROXY=$(GOPROXY) --build-arg GOSUMDB=$(GOSUMDB) --build-arg WORKER_PACKAGE=$(WORKER_PACKAGE) --build-arg VCS_REF=$(VCS_REF)
.PHONY: check image push
check:
	@test -n "$(GO_BASE_IMAGE)" -a -n "$(ALPINE_BASE_IMAGE)" -a -n "$(GOPROXY)" -a -n "$(GOSUMDB)" || (echo 'Specify internal GO_BASE_IMAGE/ALPINE_BASE_IMAGE/GOPROXY/GOSUMDB'; exit 1)
image: check
	docker buildx build --platform $(PLATFORM) --load -f deploy/docker/Dockerfile.iac-worker $(BUILD_ARGS) --target iac-worker -t $(IMAGE) .
push: check
	docker buildx build --platform $(PLATFORM) --push -f deploy/docker/Dockerfile.iac-worker $(BUILD_ARGS) --target iac-worker -t $(IMAGE) .
