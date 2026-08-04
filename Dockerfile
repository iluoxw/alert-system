ARG GO_BASE_IMAGE=golang:1.25-alpine
ARG ALPINE_BASE_IMAGE=alpine:3.22
ARG NODE_BASE_IMAGE=node:22-alpine
ARG MIGRATE_VERSION=v4.18.3

FROM ${GO_BASE_IMAGE} AS go-build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GO111MODULE=on
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GO111MODULE=$GO111MODULE \
    GOPROXY=$GOPROXY \
    GOSUMDB=$GOSUMDB
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath -buildvcs=false \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/alert-system ./cmd/server && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath -buildvcs=false \
      -ldflags="-s -w" \
      -o /out/seed-admin ./cmd/seed-admin

FROM ${ALPINE_BASE_IMAGE} AS api
ARG VERSION=dev
ARG VCS_REF=unknown
LABEL org.opencontainers.image.title="SRE Control API" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$VCS_REF \
      org.opencontainers.image.source="alert-system"
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 10001 sre-control && \
    adduser -S -D -H -u 10001 -G sre-control sre-control
WORKDIR /app
COPY --from=go-build --chown=10001:10001 /out/alert-system /app/alert-system
COPY --from=go-build --chown=10001:10001 /out/seed-admin /app/seed-admin
USER 10001:10001
EXPOSE 8080 8081
ENTRYPOINT ["/app/alert-system"]

FROM ${NODE_BASE_IMAGE} AS web-build
ARG NPM_REGISTRY=https://registry.npmjs.org
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --registry="${NPM_REGISTRY}"
COPY web ./
RUN npm run build

FROM ${ALPINE_BASE_IMAGE} AS web
ARG VERSION=dev
ARG VCS_REF=unknown
LABEL org.opencontainers.image.title="SRE Control Web" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$VCS_REF \
      org.opencontainers.image.source="alert-system"
RUN apk add --no-cache nginx && \
    mkdir -p /var/lib/nginx/logs /var/log/nginx && \
    chown -R 101:101 /var/lib/nginx /var/log/nginx
COPY deploy/docker/nginx.conf /etc/nginx/nginx.conf
COPY --chmod=0555 deploy/docker/web-entrypoint.sh /usr/local/bin/firebreak-web-entrypoint
COPY --from=web-build --chown=101:101 /src/web/dist /usr/share/nginx/html
USER 101:101
EXPOSE 8080
STOPSIGNAL SIGQUIT
ENTRYPOINT ["/usr/local/bin/firebreak-web-entrypoint"]

FROM ${GO_BASE_IMAGE} AS migrate-build
ARG TARGETOS
ARG TARGETARCH
ARG MIGRATE_VERSION
ARG GO111MODULE=on
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GO111MODULE=$GO111MODULE \
    GOPROXY=$GOPROXY \
    GOSUMDB=$GOSUMDB
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOBIN=/out \
    go install -tags=postgres github.com/golang-migrate/migrate/v4/cmd/migrate@${MIGRATE_VERSION}

FROM ${ALPINE_BASE_IMAGE} AS migrate
ARG VERSION=dev
ARG VCS_REF=unknown
LABEL org.opencontainers.image.title="SRE Control Database Migrator" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$VCS_REF \
      org.opencontainers.image.source="alert-system"
RUN apk add --no-cache ca-certificates
COPY --from=migrate-build --chown=65532:65532 /out/migrate /usr/local/bin/migrate
COPY --chown=65532:65532 migrations /migrations
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/migrate"]

# `docker build .` 默认得到可运行的 API 镜像；Web 与迁移镜像通过显式 target 构建。
FROM api AS default
