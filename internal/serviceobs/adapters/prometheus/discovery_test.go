package prometheus_test

import (
	"context"
	"net/http"
	"testing"

	"alert-system/internal/serviceobs/adapters/prometheus"
)

func TestDiscoverManifest_requiresClient(t *testing.T) {
	_, err := prometheus.DiscoverManifest(context.Background(), nil, prometheus.DiscoveryOptions{})
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestDiscoverManifest_authFailure(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := prometheus.DiscoverManifest(context.Background(), client, prometheus.DiscoveryOptions{})
	if err == nil {
		t.Fatal("expected auth error")
	}
}

func TestRedactManifestForLog(t *testing.T) {
	msg := prometheus.RedactManifestForLog(prometheus.DiscoveryManifest{
		Metrics: []prometheus.MetricMeta{{Name: "up"}},
		Labels:  []prometheus.LabelMeta{{Name: "job"}},
		Digest:  "abcdef0123456789",
	})
	if msg == "" {
		t.Fatal("expected non-empty summary")
	}
}
