package series

import (
	"math"
	"reflect"
	"testing"
)

func mkRow(values ...float64) Row {
	r := Row{Metric: map[string]string{}}
	for i := 0; i < len(values); i += 2 {
		r.Values = append(r.Values, Sample{T: values[i], V: values[i+1]})
	}
	return r
}

func TestDeltaMapResetIsNotNegative(t *testing.T) {
	// counter resets to a lower value: delta becomes the new value, not negative
	rows := []Row{mkRow(100, 50, 200, 150, 300, 5)}
	got := DeltaMap(rows)
	d := got[[2]float64{200, 300}]
	if math.Abs(d-5) > 1e-9 {
		t.Fatalf("reset interval: got %v want 5", d)
	}
	if d2 := got[[2]float64{100, 200}]; math.Abs(d2-100) > 1e-9 {
		t.Fatalf("normal interval: got %v want 100", d2)
	}
}

func TestDeltaMapSumsAcrossRows(t *testing.T) {
	rows := []Row{
		mkRow(100, 10, 200, 20),
		mkRow(100, 1, 200, 4),
	}
	got := DeltaMap(rows)
	if d := got[[2]float64{100, 200}]; math.Abs(d-13) > 1e-9 {
		t.Fatalf("sum: got %v want 13", d)
	}
}

func TestCounterDeltaEndCutoff(t *testing.T) {
	row := mkRow(100, 10, 200, 20, 300, 30)
	if d := CounterDelta(row, 200, true); math.Abs(d-10) > 1e-9 {
		t.Fatalf("cutoff at 200: got %v want 10", d)
	}
	if d := CounterDelta(row, 0, false); math.Abs(d-20) > 1e-9 {
		t.Fatalf("no cutoff: got %v want 20", d)
	}
}

func TestQueryEndCovering(t *testing.T) {
	if got := QueryEndCovering(1000, 300); got != 1300 {
		t.Fatalf("got %v want 1300", got)
	}
}

func TestCoverageBounds(t *testing.T) {
	rows := []Row{mkRow(50, 1, 100, 2, 200, 3, 260, 4, 400, 5)}
	lo, hi, ok := CoverageBounds(rows, 120, 250)
	if !ok {
		t.Fatal("expected ok")
	}
	if lo != 100 || hi != 260 {
		t.Fatalf("got lo=%v hi=%v want 100/260", lo, hi)
	}
	// no sample at/after window end: hi is the last sample
	_, hi2, _ := CoverageBounds(rows, 120, 500)
	if hi2 != 400 {
		t.Fatalf("hi past window: got %v want 400", hi2)
	}
	if _, _, ok := CoverageBounds(nil, 0, 1); ok {
		t.Fatal("empty rows must not be ok")
	}
}

func TestDropAfterAndClip(t *testing.T) {
	rows := []Row{mkRow(100, 1, 200, 2, 310, 3)}
	dropped := DropAfter(rows, 300)
	if len(dropped[0].Values) != 2 || dropped[0].Values[1].T != 200 {
		t.Fatalf("drop_after: %v", dropped[0].Values)
	}
	clipped := ClipRows(rows, 150, 260)
	if len(clipped[0].Values) != 1 || clipped[0].Values[0].T != 200 {
		t.Fatalf("clip: %v", clipped[0].Values)
	}
}

func TestCoveredSpan(t *testing.T) {
	if s := CoveredSpan(10, 30, 999); s != 20 {
		t.Fatalf("got %v", s)
	}
	if s := CoveredSpan(30, 10, 999); s != 999 {
		t.Fatalf("inverted must fall back: %v", s)
	}
}

func TestRateExtremes(t *testing.T) {
	ivs := []Interval{
		{100, 400, 900},  // 3/s, full bucket
		{400, 700, 300},  // 1/s, full bucket
		{700, 1400, 100}, // wide span: hole, excluded
		{1400, 1700, 60}, // ends after now: excluded
	}
	peak, low, ok := RateExtremes(ivs, 300, 1600, true)
	if !ok {
		t.Fatal("expected ok")
	}
	if peak.Delta != 900 || low.Delta != 300 {
		t.Fatalf("peak=%v low=%v", peak, low)
	}
	if _, _, ok := RateExtremes(ivs, 300, 0, false); !ok {
		t.Fatal("hasNow=false must include the 1400 bucket")
	}
}

func TestRouteKind(t *testing.T) {
	cases := map[string]string{
		"/system/verify_token":      "verify",
		"/system/verify_token/v2":   "verify",
		"/app/access_token":         "issue",
		"/app/access_token/refresh": "issue",
		"/healthz":                  "other",
		"":                          "other",
	}
	for route, want := range cases {
		if got := RouteKind(route); got != want {
			t.Fatalf("RouteKind(%q)=%q want %q", route, got, want)
		}
	}
}

func TestDeltaMapEmptyValues(t *testing.T) {
	got := DeltaMap([]Row{{Metric: map[string]string{}}})
	if len(got) != 0 {
		t.Fatalf("empty row: %v", got)
	}
}

func TestRowsCopySemantics(t *testing.T) {
	// DropAfter/ClipRows must not mutate the input rows
	rows := []Row{mkRow(100, 1, 200, 2, 310, 3)}
	_ = DropAfter(rows, 300)
	_ = ClipRows(rows, 150, 260)
	if len(rows[0].Values) != 3 {
		t.Fatalf("input mutated: %v", rows[0].Values)
	}
	if !reflect.DeepEqual(rows, []Row{mkRow(100, 1, 200, 2, 310, 3)}) {
		t.Fatal("input changed")
	}
}

func TestLEKey(t *testing.T) {
	if v, ok := LEKey("+Inf"); ok {
		t.Fatalf("+Inf must be !ok, got %v", v)
	}
	if v, ok := LEKey("inf"); ok {
		t.Fatalf("inf must be !ok, got %v", v)
	}
	if v, ok := LEKey(" 0.025 "); !ok || v != 0.025 {
		t.Fatalf("0.025: %v %v", v, ok)
	}
	if _, ok := LEKey("junk"); ok {
		t.Fatal("junk must be !ok")
	}
}

func TestHistogramQuantile(t *testing.T) {
	// buckets: le 0.01=>100, 0.025=>800, 0.1=>990, 1=>1000
	buckets := []Bucket{{0.01, 100}, {0.025, 800}, {0.1, 990}, {1, 1000}}
	// p99: rank 990 lands exactly on le=0.1 bucket boundary (c >= rank)
	q99, ok := HistogramQuantile(0.99, buckets, 1000)
	if !ok || math.Abs(q99-0.1) > 1e-9 {
		t.Fatalf("p99: %v %v", q99, ok)
	}
	// p50: rank 500 inside (0.01,0.025] span 700: 0.01 + 0.015*400/700
	q50, ok := HistogramQuantile(0.5, buckets, 1000)
	if !ok || math.Abs(q50-(0.01+0.015*400.0/700.0)) > 1e-9 {
		t.Fatalf("p50: %v", q50)
	}
	// rank beyond the last finite bucket (total came from +Inf): clamp to last le
	limited := []Bucket{{0.01, 100}, {0.025, 800}, {0.1, 990}}
	q999, ok := HistogramQuantile(0.999, limited, 1000)
	if !ok || q999 != 0.1 {
		t.Fatalf("clamp: %v", q999)
	}
	// rank inside the last bucket interpolates
	qIn, ok := HistogramQuantile(0.9999, buckets, 1000)
	if !ok || math.Abs(qIn-0.991) > 1e-9 {
		t.Fatalf("in-bucket: %v", qIn)
	}
	if _, ok := HistogramQuantile(0.5, nil, 100); ok {
		t.Fatal("empty buckets")
	}
	if _, ok := HistogramQuantile(0.5, buckets, 0); ok {
		t.Fatal("zero total")
	}
}

func TestGaugeMax(t *testing.T) {
	rows := []Row{mkRow(100, 3, 200, 7), mkRow(100, 5, 300, 2)}
	if v, ok := GaugeMax(rows); !ok || v != 7 {
		t.Fatalf("got %v %v", v, ok)
	}
	if _, ok := GaugeMax(nil); ok {
		t.Fatal("empty")
	}
}
