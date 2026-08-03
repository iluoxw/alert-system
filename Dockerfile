ARG GO_VERSION=1.25
ARG NODE_VERSION=22
ARG ALPINE_VERSION=3.22
ARG NGINX_VERSION=1.27-alpine
ARG MIGRATE_VERSION=v4.18.3

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS go-build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
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

FROM alpine:${ALPINE_VERSION} AS api
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

FROM node:${NODE_VERSION}-alpine AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY web ./
RUN npm run build

FROM nginx:${NGINX_VERSION} AS web
ARG VERSION=dev
ARG VCS_REF=unknown
LABEL org.opencontainers.image.title="SRE Control Web" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$VCS_REF \
      org.opencontainers.image.source="alert-system"
RUN rm -f /etc/nginx/conf.d/default.conf
COPY deploy/docker/nginx.conf /etc/nginx/nginx.conf
COPY --from=web-build --chown=101:101 /src/web/dist /usr/share/nginx/html
USER 101:101
EXPOSE 8080
STOPSIGNAL SIGQUIT

FROM migrate/migrate:${MIGRATE_VERSION} AS migrate
ARG VERSION=dev
ARG VCS_REF=unknown
LABEL org.opencontainers.image.title="SRE Control Database Migrator" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$VCS_REF \
      org.opencontainers.image.source="alert-system"
COPY --chown=65532:65532 migrations /migrations
USER 65532:65532

# `docker build .` 默认得到可运行的 API 镜像；Web 与迁移镜像通过显式 target 构建。
FROM api AS default
