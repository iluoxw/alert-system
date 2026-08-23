package prometheus_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"alert-system/internal/serviceobs/adapters/prometheus"
	"alert-system/internal/serviceobs/observe"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func TestParseResponse_vector(t *testing.T) {
	parsed, err := prometheus.ParseQueryResponse(loadFixture(t, "vector_success.json"), 10)
	if err != nil {
		t.Fatalf("ParseQueryResponse: %v", err)
	}
	if parsed.ResultType != "vector" || len(parsed.Series) != 2 {
		t.Fatalf("parsed = %+v", parsed)
	}
	if parsed.Series[0].Labels["instance"] != "a" || parsed.Series[0].Value != 42.5 {
		t.Fatalf("stable sort broken: %+v", parsed.Series)
	}
	if parsed.Series[1].Value != 12 {
		t.Fatalf("value = %v", parsed.Series[1].Value)
	}
}

func TestParseResponse_scalar(t *testing.T) {
	parsed, err := prometheus.ParseQueryResponse(loadFixture(t, "scalar_success.json"), 10)
	if err != nil {
		t.Fatalf("ParseQueryResponse: %v", err)
	}
	if parsed.Scalar == nil || parsed.Scalar.Value != 3.14 {
		t.Fatalf("scalar = %+v", parsed.Scalar)
	}
}

func TestParseResponse_matrix(t *testing.T) {
	parsed, err := prometheus.ParseQueryResponse(loadFixture(t, "matrix_success.json"), 10)
	if err != nil {
		t.Fatalf("ParseQueryResponse: %v", err)
	}
	if len(parsed.Series) != 1 || len(parsed.Series[0].Points) != 3 {
		t.Fatalf("matrix = %+v", parsed.Series)
	}
}

func TestParseResponse_warningsPreserved(t *testing.T) {
	parsed, err := prometheus.ParseQueryResponse(loadFixture(t, "vector_with_warnings.json"), 10)
	if err != nil {
		t.Fatalf("ParseQueryResponse: %v", err)
	}
	if len(parsed.Warnings) != 1 || parsed.Warnings[0] == "" {
		t.Fatalf("warnings = %#v", parsed.Warnings)
	}
}

func TestParseResponse_rejectsNaN(t *testing.T) {
	_, err := prometheus.ParseQueryResponse(loadFixture(t, "vector_nan.json"), 10)
	if !errors.Is(err, prometheus.ErrInvalidSample) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseResponse_rejectsInf(t *testing.T) {
	_, err := prometheus.ParseQueryResponse(loadFixture(t, "vector_inf.json"), 10)
	if !errors.Is(err, prometheus.ErrInvalidSample) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseResponse_rejectsStringResultType(t *testing.T) {
	_, err := prometheus.ParseQueryResponse(loadFixture(t, "string_result.json"), 10)
	if !errors.Is(err, prometheus.ErrUnsupportedResult) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseResponse_rejectsCardinality(t *testing.T) {
	_, err := prometheus.ParseQueryResponse(loadFixture(t, "vector_success.json"), 1)
	if !errors.Is(err, prometheus.ErrCardinalityExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestMapHTTPStatus_auth(t *testing.T) {
	if !errors.Is(prometheus.MapHTTPStatus(401), observe.ErrUpstreamAuthFailed) {
		t.Fatal("401 should map to auth failed")
	}
	if !errors.Is(prometheus.MapHTTPStatus(403), observe.ErrUpstreamAuthFailed) {
		t.Fatal("403 should map to auth failed")
	}
}

func TestMapHTTPStatus_rateLimit(t *testing.T) {
	if !errors.Is(prometheus.MapHTTPStatus(429), observe.ErrUpstreamRateLimited) {
		t.Fatal("429 should map to rate limited")
	}
}

func TestMapHTTPStatus_5xx(t *testing.T) {
	err := prometheus.MapHTTPStatus(503)
	if !errors.Is(err, prometheus.ErrUpstreamReadFailed) {
		t.Fatalf("503 err = %v", err)
	}
}

func TestMapHTTPStatus_2xx(t *testing.T) {
	if err := prometheus.MapHTTPStatus(200); err != nil {
		t.Fatalf("200 should be nil, got %v", err)
	}
}
