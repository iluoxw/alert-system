package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/agentdelivery"
	"alert-system/internal/api"
	"alert-system/internal/config"
	"alert-system/internal/iacccloudproxy"
	"alert-system/internal/omaruntime"
)

const testAnalysisSource = "e4628d3d-3a35-4313-91d7-869756c7ae23"

type hostTestWorker struct {
	ready atomic.Bool
	run   func(context.Context)
}

func (w *hostTestWorker) Run(ctx context.Context) { w.run(ctx) }
func (w *hostTestWorker) Ready() bool             { return w.ready.Load() }

func testAnalysisValues() map[string]string {
	return map[string]string{"OMA_ENABLED": "true", "OMA_API_KEY": "private-test-value", "OMA_AGENT_ID": "agent_test", "OMA_ENVIRONMENT_ID": "env_test", "OMA_SOURCE_IDS": testAnalysisSource, "OMA_ROLE": "iac-review"}
}

func testDelivery() *agentdelivery.ServerConfig {
	return &agentdelivery.ServerConfig{Routing: agentdelivery.Routing{SourceIDs: []string{testAnalysisSource}, Fallback: agentdelivery.Rule{Role: "iac-review"}}}
}

func TestHostAnalysisDefaultOffDoesNotReadCredentialsOrConstruct(t *testing.T) {
	for _, value := range []string{"", "false"} {
		get := func(key string) string {
			if key != "OMA_ENABLED" {
				t.Fatalf("disabled runtime read %s", key)
			}
			return value
		}
		worker, err := configureAnalysisWithFactory(get, nil, func(omaruntime.Config) (analysisWorker, error) {
			t.Fatal("disabled runtime constructed a worker")
			return nil, nil
		})
		if worker != nil || err != nil {
			t.Fatal("disabled analysis must be a no-op", err)
		}
	}
	if run := startAnalysis(context.Background(), nil); run != nil {
		t.Fatal("disabled analysis started")
	}
}

func TestHostAnalysisRequiresDeliverySourceAndRoutedRole(t *testing.T) {
	for _, scenario := range []string{"invalid-enabled", "missing-delivery", "missing-model-config", "unknown-source", "unrouted-role", "constructor-error", "valid-without-machine-credential"} {
		t.Run(scenario, func(t *testing.T) {
			values := testAnalysisValues()
			delivery := testDelivery()
			switch scenario {
			case "invalid-enabled":
				values["OMA_ENABLED"] = "private-test-value"
			case "missing-delivery":
				delivery = nil
			case "missing-model-config":
				delete(values, "OMA_API_KEY")
			case "unknown-source":
				values["OMA_SOURCE_IDS"] = "3c2308de-2938-49f1-a224-3ce9df417fad"
			case "unrouted-role":
				values["OMA_ROLE"] = "another-role"
			}
			called := 0
			worker, err := configureAnalysisWithFactory(func(key string) string { return values[key] }, delivery, func(c omaruntime.Config) (analysisWorker, error) {
				called++
				if c.Broker.Binary != "/app/iacctl-go" || c.Worker.Principal.Role != "iac-review" {
					t.Fatal("host changed runtime contract")
				}
				if scenario == "constructor-error" {
					return nil, errors.New("fixed dependency unavailable")
				}
				return &hostTestWorker{}, nil
			})
			if scenario == "valid-without-machine-credential" {
				if err != nil || worker == nil || called != 1 {
					t.Fatal("internal consumer wrongly required a new machine bearer", err)
				}
			} else if err == nil || worker != nil || strings.Contains(err.Error(), "private-test-value") {
				t.Fatal("invalid config accepted or leaked", err)
			}
			if scenario != "valid-without-machine-credential" && scenario != "constructor-error" && called != 0 {
				t.Fatal("invalid host config reached constructor")
			}
		})
	}
	values := testAnalysisValues()
	delivery := testDelivery()
	delivery.Routing.Fallback.Role = "default-role"
	delivery.Routing.Routes = []agentdelivery.Rule{{Role: "iac-review"}}
	if _, err := configureAnalysisWithFactory(func(k string) string { return values[k] }, delivery, func(omaruntime.Config) (analysisWorker, error) { return &hostTestWorker{}, nil }); err != nil {
		t.Fatal("explicit non-fallback role rejected", err)
	}
}

func TestHostAnalysisStopWaitsBeforeSharedStoreClose(t *testing.T) {
	started, canceled, settle, hostDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var storeClosed atomic.Bool
	worker := &hostTestWorker{run: func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-settle
		if storeClosed.Load() {
			t.Error("shared store closed before analysis settled")
		}
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		defer close(hostDone)
		defer storeClosed.Store(true)
		run := startAnalysis(ctx, worker)
		defer run.stop()
		<-started
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("host stop did not cancel analysis")
	}
	select {
	case <-hostDone:
		t.Fatal("host did not wait for analysis settle")
	default:
	}
	if storeClosed.Load() {
		t.Fatal("shared store closed while analysis was still settling")
	}
	close(settle)
	select {
	case <-hostDone:
	case <-time.After(time.Second):
		t.Fatal("host did not exit after analysis settled")
	}
	if !storeClosed.Load() {
		t.Fatal("host did not close its store after analysis completed")
	}
}

func hostTestRouter() *api.Router {
	return api.NewRouter(config.Config{}, nil, nil, nil, nil, nil, http.NotFoundHandler(), nil)
}

func hostRequest(router http.Handler, method, path string) int {
	out := httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest(method, path, nil))
	return out.Code
}

func TestHostAnalysisReadinessIndependentOfCoreAndHistoryAlwaysRegistered(t *testing.T) {
	for _, active := range []bool{false, true} {
		router := hostTestRouter()
		router.RegisterHealthRoutes(func(context.Context) error { return nil })
		worker := &hostTestWorker{}
		if active {
			registerAnalysisRoutes(router, worker)
		} else {
			registerAnalysisRoutes(router, nil)
		}
		if code := hostRequest(router, "GET", "/api/v1/admin/analysis-runs"); code != 401 {
			t.Fatal("analysis history route missing or anonymous access allowed", code)
		}
		if code := hostRequest(router, "GET", "/readyz"); code != 200 {
			t.Fatal("analysis state altered core readiness", code)
		}
		want := 404
		if active {
			want = 503
		}
		if code := hostRequest(router, "GET", "/readyz/analysis"); code != want {
			t.Fatal("analysis ready before real progress or disabled route exposed", code)
		}
		if active {
			worker.ready.Store(true)
			if code := hostRequest(router, "GET", "/readyz/analysis"); code != 200 {
				t.Fatal("analysis ready not reflected", code)
			}
			worker.ready.Store(false)
			if code := hostRequest(router, "GET", "/readyz"); code != 200 {
				t.Fatal("failed optional analysis removed core API", code)
			}
		}
	}
}

func TestHostCCloudProxyOptInAndFailClosed(t *testing.T) {
	for _, flag := range []string{"", "false"} {
		proxy, err := configureCCloudProxy(func(key string) string {
			if key != "IAC_CCLOUD_PROXY_ENABLED" {
				t.Fatalf("disabled proxy read %s", key)
			}
			return flag
		})
		if err != nil || proxy != nil {
			t.Fatal("disabled proxy initialized", err)
		}
	}
	for _, missing := range []string{"IAC_CCLOUD_PROXY_TOKEN", "IAC_CCLOUD_UPSTREAM_KEY", ""} {
		values := map[string]string{"IAC_CCLOUD_PROXY_ENABLED": "true", "IAC_CCLOUD_PROXY_TOKEN": strings.Repeat("t", 32), "IAC_CCLOUD_UPSTREAM_KEY": "private-test-value"}
		delete(values, missing)
		proxy, err := configureCCloudProxy(func(key string) string { return values[key] })
		if missing != "" {
			if err == nil || proxy != nil || strings.Contains(err.Error(), "private-test-value") {
				t.Fatal("missing credential accepted or leaked", err)
			}
			continue
		}
		if err != nil || proxy == nil {
			t.Fatal("valid explicit proxy config rejected", err)
		}
		router := hostTestRouter()
		registerCCloudProxy(router, proxy)
		if code := hostRequest(router, "GET", iacccloudproxy.Prefix+"/cloud/api/v1/agent/meta/resources"); code != 401 {
			t.Fatal("proxy route missing or unauthenticated upstream access", code)
		}
		request := httptest.NewRequest("GET", iacccloudproxy.Prefix+"/cloud/api/v1/agent/meta/resources", nil)
		request.Header.Set("Authorization", "Bearer "+values["IAC_CCLOUD_PROXY_TOKEN"])
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatal("proxy metadata request failed", response.Code)
		}
	}
}
