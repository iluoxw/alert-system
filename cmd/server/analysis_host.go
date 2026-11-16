package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"alert-system/internal/agentdelivery"
	"alert-system/internal/api"
	"alert-system/internal/iacccloudproxy"
	"alert-system/internal/omaruntime"
	"alert-system/internal/store"
)

type analysisWorker interface {
	Run(context.Context)
	Ready() bool
}

func enabled(get func(string) string, key string) (bool, error) {
	value := get(key)
	if value == "" {
		return false, nil
	}
	yes, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s 必须为布尔值", key)
	}
	return yes, nil
}

func configureAnalysis(get func(string) string, delivery *agentdelivery.ServerConfig, st *store.Store) (analysisWorker, error) {
	return configureAnalysisWithFactory(get, delivery, func(c omaruntime.Config) (analysisWorker, error) {
		return omaruntime.NewConfiguredWorker(st, c)
	})
}

func configureAnalysisWithFactory(get func(string) string, delivery *agentdelivery.ServerConfig, create func(omaruntime.Config) (analysisWorker, error)) (analysisWorker, error) {
	yes, err := enabled(get, "OMA_ENABLED")
	if err != nil || !yes {
		return nil, err
	}
	if delivery == nil {
		return nil, errors.New("启用 OMA 需要已有 Agent delivery 配置")
	}
	c, err := omaruntime.LoadConfig(get)
	if err != nil {
		return nil, err
	}
	for _, id := range c.Worker.SourceIDs {
		if !slices.Contains(delivery.Routing.SourceIDs, id) {
			return nil, errors.New("OMA 来源必须包含在 Agent delivery 允许范围内")
		}
	}
	role := c.Worker.Principal.Role
	roleConfigured := delivery.Routing.Fallback.Role == role
	for _, route := range delivery.Routing.Routes {
		roleConfigured = roleConfigured || route.Role == role
	}
	if !roleConfigured {
		return nil, errors.New("OMA 角色必须有明确的 Agent delivery 路由")
	}
	// 进程内消费者通过共享 Store 原子交接收据，不新增机器 HTTP token。
	return create(c)
}

type runningAnalysis struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startAnalysis(ctx context.Context, worker analysisWorker) *runningAnalysis {
	if worker == nil {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	run := &runningAnalysis{cancel: cancel, done: make(chan struct{})}
	go func() { defer close(run.done); worker.Run(workerCtx) }()
	return run
}

func (run *runningAnalysis) stop() {
	if run == nil {
		return
	}
	run.cancel()
	// 必须等待在途请求取消和持久化 settle，宿主才能关闭共享连接池。
	<-run.done
}

func registerAnalysisRoutes(router *api.Router, worker analysisWorker) {
	// 关闭消费者后，管理员仍可查看历史结果；权限与只读事务由 API 层负责。
	router.RegisterAnalysisResultRoutes(omaruntime.NewRepository())
	if worker != nil {
		router.MountUnauthenticated(http.MethodGet, "/readyz/analysis", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if !worker.Ready() {
				http.Error(w, "analysis worker progress unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
	}
}

func configureCCloudProxy(get func(string) string) (*iacccloudproxy.Handler, error) {
	yes, err := enabled(get, "IAC_CCLOUD_PROXY_ENABLED")
	if err != nil || !yes {
		return nil, err
	}
	return iacccloudproxy.New(iacccloudproxy.Options{
		ProxyToken:  get("IAC_CCLOUD_PROXY_TOKEN"),
		CloudAPIKey: get("IAC_CCLOUD_UPSTREAM_KEY"),
	})
}

func registerCCloudProxy(router *api.Router, proxy *iacccloudproxy.Handler) {
	if proxy != nil {
		// 此端点独立验证窄权限代理 token，不能落入管理员 session 路由。
		router.MountUnauthenticated(http.MethodGet, iacccloudproxy.Prefix+"/*path", proxy)
	}
}
