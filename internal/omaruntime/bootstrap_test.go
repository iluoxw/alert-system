package omaruntime

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigUsesHostAccessorAndValidatesWithoutIO(t *testing.T) {
	values := map[string]string{"OMA_API_KEY": "private-test-value", "OMA_AGENT_ID": "agent_test", "OMA_ENVIRONMENT_ID": "env_test", "OMA_SOURCE_IDS": "e4628d3d-3a35-4313-91d7-869756c7ae23", "OMA_ROLE": "iac-review"}
	get := func(k string) string { return values[k] }
	c, e := LoadConfig(get)
	if e != nil {
		t.Fatal(e)
	}
	if c.Worker.Concurrency != 1 || c.Worker.Lease != time.Minute || c.Broker.Binary != "/app/iacctl-go" || c.Model.APIKey != values["OMA_API_KEY"] {
		t.Fatal("unexpected assembly defaults")
	}
	values["OMA_WORKERS"] = "9"
	if _, e = LoadConfig(get); e == nil {
		t.Fatal("unbounded concurrency accepted")
	}
	values["OMA_WORKERS"] = "1"
	values["OMA_RUN_TIMEOUT"] = "private-test-value"
	if _, e = LoadConfig(get); e == nil || strings.Contains(e.Error(), "private-test-value") {
		t.Fatal("invalid duration was accepted or leaked")
	}
}
