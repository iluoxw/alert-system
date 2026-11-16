package prom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPromInstantParsesVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/v1/query") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"route":"/system/verify_token","result":"success"},"value":[1790000000.5,"123"]},
			{"metric":{"route":"/x"},"value":[1790000000.5,"NaN"]}]}}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "/api/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.Prom(context.Background(), "up", 1790000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("NaN row must be dropped; got %d rows", len(rows))
	}
	if rows[0].Values[0].V != 123 {
		t.Fatalf("value: %v", rows[0].Values[0])
	}
	if rows[0].Metric["route"] != "/system/verify_token" {
		t.Fatalf("metric: %v", rows[0].Metric)
	}
}

func TestPromRangeParsesMatrix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/v1/query_range") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("step") != "300" {
			t.Errorf("step: %s", r.URL.Query().Get("step"))
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"result":"success"},"values":[[100,"10"],[400,"20"],[700,"5"]]}]}}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "/api/v1", "", "")
	rows, err := c.PromRange(context.Background(), "m", 0, 1000, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Values) != 3 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[0].Values[2].V != 5 {
		t.Fatalf("last value: %+v", rows[0].Values[2])
	}
}

func TestAuthErrors(t *testing.T) {
	// missing env
	os.Unsetenv("TEST_X_AUTH")
	if _, err := New("https://x", "/api/v1", "", "TEST_X_AUTH"); err == nil {
		t.Fatal("missing env must be AuthError")
	} else if _, ok := err.(*AuthError); !ok {
		t.Fatalf("want AuthError, got %T", err)
	}
	// malformed
	os.Setenv("TEST_X_AUTH", "nocolon")
	defer os.Unsetenv("TEST_X_AUTH")
	if _, err := New("https://x", "/api/v1", "", "TEST_X_AUTH"); err == nil {
		t.Fatal("malformed must be AuthError")
	}
	// 401 from server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	os.Setenv("TEST_X_AUTH", "u:p")
	c, _ := New(srv.URL, "/api/v1", "", "TEST_X_AUTH")
	if _, err := c.Prom(context.Background(), "up", 0); err == nil {
		t.Fatal("401 must error")
	} else if _, ok := err.(*AuthError); !ok {
		t.Fatalf("401 must be AuthError, got %T: %v", err, err)
	}
}

func TestQueryErrorOnHTTP500AndBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "q500") {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"status":"error","error":"parse error"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "/api/v1", "", "")
	if _, err := c.PromRange(context.Background(), "q500", 0, 1, 60); err == nil {
		t.Fatal("500 must error")
	} else if _, ok := err.(*QueryError); !ok {
		t.Fatalf("500 must be QueryError, got %T", err)
	}
	if _, err := c.Prom(context.Background(), "x", 0); err == nil {
		t.Fatal("status=error must fail")
	} else if _, ok := err.(*QueryError); !ok {
		t.Fatalf("want QueryError, got %T", err)
	}
}

func TestSearchSQLBody(t *testing.T) {
	var gotBody map[string]any
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewDecoder(r.Body).Decode(&gotBody)
		if r.Header.Get("Authorization") == "" {
			t.Error("missing auth header")
		}
		w.Write([]byte(`{"total":1,"hits":[{"m":"2026-11-11T22:00:00","c":"3"}]}`))
	}))
	defer srv.Close()
	os.Setenv("TEST_S_AUTH", "u:p")
	defer os.Unsetenv("TEST_S_AUTH")
	c, _ := New(srv.URL, "/api/org/prometheus/api/v1", "/api/org/_search", "TEST_S_AUTH")
	out, err := c.SearchSQL(context.Background(), "logs", "SELECT 1", 100, 200, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "type=logs") {
		t.Fatalf("query: %s", gotQuery)
	}
	q := gotBody["query"].(map[string]any)
	if q["start_time"] != float64(100*1_000_000) {
		t.Fatalf("start_time: %v", q["start_time"])
	}
	if q["timezone"] != "+08:00" {
		t.Fatalf("timezone: %v", q["timezone"])
	}
	if out["total"] != float64(1) {
		t.Fatalf("out: %v", out)
	}
}

func TestSearchUnsupportedOnThanos(t *testing.T) {
	c, _ := New("https://thanos", "/api/v1", "", "")
	if _, err := c.SearchSQL(context.Background(), "logs", "SELECT 1", 0, 1, 10); err == nil {
		t.Fatal("thanos client must refuse SQL search")
	}
}

func TestScalarResultIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1794410464,"1"]}}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "", "")
	rows, err := c.Prom(context.Background(), "1", 1794410464)
	if err != nil {
		t.Fatalf("scalar must not error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("scalar has no rows: %v", rows)
	}
}

func TestOMAPlaceholderPassesVerbatim(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	os.Setenv("TEST_P_AUTH", "oma_ph_exampleplaceholder")
	defer os.Unsetenv("TEST_P_AUTH")
	c, err := New(srv.URL, "", "", "TEST_P_AUTH")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prom(context.Background(), "up", 0); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "oma_ph_exampleplaceholder" {
		t.Fatalf("placeholder must pass verbatim, got %q", gotAuth)
	}
}
