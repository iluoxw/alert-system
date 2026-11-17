package omaruntime

import (
	"alert-system/internal/agentdelivery"
	"alert-system/internal/iacbroker"
	"alert-system/internal/store"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"strconv"
	"strings"
	"time"
)

// Config is the optional runtime's assembly configuration. It contains secrets;
// callers must not serialize or log it. The host process owns its store and lifecycle.
type Config struct {
	Worker     Options
	Model      HTTPModelConfig
	Broker     iacbroker.Options
	HealthAddr string
}

// LoadConfig accepts the host's platform configuration accessor; it never reads a
// credential file or creates external infrastructure. The host explicitly opts in.
func LoadConfig(get func(string) string) (Config, error) {
	var c Config
	if get == nil {
		return c, errors.New("oma runtime: missing configuration accessor")
	}
	env := func(key, def string) string {
		if s := get(key); s != "" {
			return s
		}
		return def
	}
	for _, key := range []string{"OMA_API_KEY", "OMA_AGENT_ID", "OMA_ENVIRONMENT_ID", "OMA_SOURCE_IDS", "OMA_ROLE"} {
		if get(key) == "" {
			return c, fmt.Errorf("required environment variable %s is missing", key)
		}
	}
	workers, e := strconv.Atoi(env("OMA_WORKERS", "1"))
	if e != nil {
		return c, errors.New("invalid OMA_WORKERS")
	}
	times := make([]time.Duration, 4)
	for i, item := range [][2]string{{"OMA_POLL_INTERVAL", "1s"}, {"OMA_LEASE", "60s"}, {"OMA_ROUND_TIMEOUT", "120s"}, {"OMA_RUN_TIMEOUT", "10m"}} {
		times[i], e = time.ParseDuration(env(item[0], item[1]))
		if e != nil {
			return c, fmt.Errorf("invalid %s", item[0])
		}
	}
	c.Worker = Options{QueryProfile: iacbroker.Profile, Principal: agentdelivery.Principal{ClientID: env("OMA_CLIENT_ID", "oma-worker"), Role: get("OMA_ROLE")}, WorkerID: env("HOSTNAME", "oma-worker") + ":" + uuid.NewString(), SourceIDs: strings.Split(get("OMA_SOURCE_IDS"), ","), QueryIDs: iacbroker.QueryIDs(), Concurrency: workers, PollInterval: times[0], Lease: times[1], RoundTimeout: times[2], RunTimeout: times[3]}
	if e = c.Worker.validate(); e != nil {
		return Config{}, e
	}
	c.Model = HTTPModelConfig{BaseURL: env("OMA_BASE_URL", "https://oma.test.seewo.com"), APIKey: get("OMA_API_KEY"), AgentID: get("OMA_AGENT_ID"), EnvironmentID: get("OMA_ENVIRONMENT_ID")}
	c.Broker = iacbroker.Options{Binary: env("IACCTL_BINARY", "/app/iacctl-go"), SourceRevision: env("IACCTL_SOURCE_REVISION", "643ddf7aaec314a438f052a719b3741f2eef71ff"), Timeout: 45 * time.Second}
	if flag := get("IAC_CCLOUD_PROXY_ENABLED"); flag != "" {
		enabled, e := strconv.ParseBool(flag)
		if e != nil {
			return Config{}, errors.New("invalid IAC_CCLOUD_PROXY_ENABLED")
		}
		if enabled {
			c.Broker.CCloud = &iacbroker.CCloudOptions{NodeBinary: env("IAC_CCLOUD_NODE_BINARY", "/usr/local/bin/node"), PackageDir: env("IAC_CCLOUD_PACKAGE_DIR", "/opt/ccloud-cli"), ProxyToken: get("IAC_CCLOUD_PROXY_TOKEN")}
			c.Worker.CCloudL4 = true
			c.Worker.QueryProfile = iacbroker.ProfileWithCCloud
		}
	}
	c.HealthAddr = env("OMA_HEALTH_ADDR", ":8080")
	return c, nil
}

// NewConfiguredWorker shares the API's existing store and queue protocol. It does
// not start goroutines, open a listener, or own/close the store. The host calls
// Run(ctx), waits for it on shutdown, and can expose Ready on its own health route.
func NewConfiguredWorker(st *store.Store, c Config) (*Worker, error) {
	model, e := NewHTTPModel(c.Model)
	if e != nil {
		return nil, e
	}
	broker, e := iacbroker.New(c.Broker)
	if e != nil {
		return nil, errors.New("fixed read-only observation broker unavailable")
	}
	collector := func(ctx context.Context, ids []string) (json.RawMessage, error) {
		bundle, e := broker.Collect(ctx, ids)
		if e != nil {
			return nil, e
		}
		return json.Marshal(bundle)
	}
	return NewWorker(st, agentdelivery.NewQueue(st), model, collector, c.Worker)
}
