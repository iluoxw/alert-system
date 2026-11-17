package omaruntime

import (
	"slices"
	"testing"
)

func TestCCloudConfigDefaultOffPreservesLegacyProfile(t *testing.T) {
	values := map[string]string{"OMA_API_KEY": "test", "OMA_AGENT_ID": "agent_test", "OMA_ENVIRONMENT_ID": "env_test", "OMA_SOURCE_IDS": "e4628d3d-3a35-4313-91d7-869756c7ae23", "OMA_ROLE": "iac-review"}
	for _, flag := range []string{"", "false", "true"} {
		t.Run(flag, func(t *testing.T) {
			values["IAC_CCLOUD_PROXY_ENABLED"] = flag
			c, e := LoadConfig(func(k string) string {
				if k == "IAC_CCLOUD_UPSTREAM_KEY" {
					t.Fatal("worker accessed upstream credential")
				}
				return values[k]
			})
			if e != nil {
				t.Fatal(e)
			}
			enabled := flag == "true"
			if c.Worker.CCloudL4 != enabled || (c.Broker.CCloud != nil) != enabled || slices.Contains(c.Worker.QueryProfile("iac-sre-oma-canary"), "ccloud_l4") != enabled {
				t.Fatal("capability/profile mismatch")
			}
			if enabled && (c.Broker.CCloud.NodeBinary != "/usr/local/bin/node" || c.Broker.CCloud.PackageDir != "/opt/ccloud-cli" || c.Broker.CCloud.ProxyToken != "") {
				t.Fatal("default package config changed or missing token silently disabled capability")
			}
		})
	}
}
