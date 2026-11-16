// Package series implements the local counter-differencing pipeline shared by
// every metric source. The rules are ported verbatim from the Python skill
// (golang/skills/iac-go-inspection/references/queries.md):
//
//   - counters are differenced locally from query_range samples; never
//     increase() and never subqueries (O2 undercounts increase() on sparse
//     series and returns wrong subquery results);
//   - a negative diff is a reset, not a negative: add the new value instead;
//   - counter queries reach one step past the window end (O2 drops the sample
//     when the requested end sits on the step grid);
//   - samples newer than the fetch time are dropped (O2 future-fills the last
//     grid point from the last scrape);
//   - the qps denominator is the span the differences actually cover, not the
//     requested window.
package series

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// Sample is one parsed point of a range series.
type Sample struct {
	T float64
	V float64
}

// Row is one Prometheus range series with only valid samples.
type Row struct {
	Metric map[string]string
	Values []Sample
}

// Interval is one differencing bucket of a counter.
type Interval struct {
	Start float64
	End   float64
	Delta float64
}

// DeltaMap sums positive counter deltas keyed by (t0, t1) across all rows.
// A drop is a reset, not a negative: the new value is added instead.
func DeltaMap(rows []Row) map[[2]float64]float64 {
	acc := make(map[[2]float64]float64)
	for _, row := range rows {
		for i := 1; i < len(row.Values); i++ {
			t0, v0 := row.Values[i-1].T, row.Values[i-1].V
			t1, v1 := row.Values[i].T, row.Values[i].V
			d := v1 - v0
			if d < 0 {
				d = v1
			}
			acc[[2]float64{t0, t1}] += d
		}
	}
	return acc
}

// CounterDelta sums positive diffs on a single series. When hasEnd is set,
// only intervals that finish by endAt count.
func CounterDelta(row Row, endAt float64, hasEnd bool) float64 {
	total := 0.0
	for i := 1; i < len(row.Values); i++ {
		t1 := row.Values[i].T
		if hasEnd && t1 > endAt+1e-3 {
			continue
		}
		d := row.Values[i].V - row.Values[i-1].V
		if d < 0 {
			d = row.Values[i].V
		}
		total += d
	}
	return total
}

// QueryEndCovering returns end+step. OpenObserve drops the sample when the
// requested end sits on the step grid, so a counter query that stops at the
// window end can miss up to one step. On standard Prometheus the extra point
// is clipped by CoverageBounds/ClipRows, so reaching past is harmless.
func QueryEndCovering(end float64, step int) float64 {
	return end + float64(step)
}

// CoverageBounds returns the first sample at or before the window and the
// first sample at or after it: the span the counter differences actually
// cover. ok is false when no valid sample exists.
func CoverageBounds(rows []Row, winStart, winEnd float64) (lo, hi float64, ok bool) {
	var times []float64
	for _, row := range rows {
		for _, s := range row.Values {
			times = append(times, s.T)
		}
	}
	if len(times) == 0 {
		return 0, 0, false
	}
	sort.Float64s(times)
	uniq := times[:1]
	for _, t := range times[1:] {
		if math.Abs(t-uniq[len(uniq)-1]) > 1e-6 {
			uniq = append(uniq, t)
		}
	}
	lo = uniq[0]
	for _, t := range uniq {
		if t <= winStart+1e-3 {
			lo = t
		}
	}
	hi = uniq[len(uniq)-1]
	for _, t := range uniq {
		if t+1e-3 >= winEnd {
			hi = t
			break
		}
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi, true
}

// DropAfter removes samples newer than limit. A query that asks for one step
// past "now" still returns that grid point; OpenObserve fills it from the last
// scrape, so the timestamp gap is a full step while the counter has only moved
// for a few seconds.
func DropAfter(rows []Row, limit float64) []Row {
	out := make([]Row, 0, len(rows))
	for _, row := range rows {
		vals := row.Values[:0:0]
		for _, s := range row.Values {
			if s.T > limit+1.0 {
				continue
			}
			vals = append(vals, s)
		}
		if len(vals) == 0 {
			continue
		}
		r := row
		r.Values = vals
		out = append(out, r)
	}
	return out
}

// ClipRows keeps samples inside [lo, hi]. Gauges are not passed through here.
func ClipRows(rows []Row, lo, hi float64) []Row {
	out := make([]Row, 0, len(rows))
	for _, row := range rows {
		vals := row.Values[:0:0]
		for _, s := range row.Values {
			if s.T+1e-3 < lo || s.T-1e-3 > hi {
				continue
			}
			vals = append(vals, s)
		}
		if len(vals) == 0 {
			continue
		}
		r := row
		r.Values = vals
		out = append(out, r)
	}
	return out
}

// CoveredSpan is the seconds the differences actually cover; fallback when
// the bounds are unknown or inverted.
func CoveredSpan(lo, hi, fallback float64) float64 {
	if hi <= lo {
		return fallback
	}
	return hi - lo
}

// RateExtremes returns the peak and low among step-wide buckets the samples
// fully cover. A wider span is a hole (a counter that skipped grid points),
// not a 5-minute rate; a bucket that finishes after nowTs is the unfinished
// edge of a live window. ok is false when no full bucket remains.
func RateExtremes(ivs []Interval, step int, nowTs float64, hasNow bool) (peak, low Interval, ok bool) {
	width := float64(step)
	var usable []Interval
	for _, iv := range ivs {
		span := iv.End - iv.Start
		if span <= 0 || math.Abs(span-width) > 1.0 {
			continue
		}
		if hasNow && iv.End > nowTs+1.0 {
			continue
		}
		usable = append(usable, iv)
	}
	if len(usable) == 0 {
		return Interval{}, Interval{}, false
	}
	peak, low = usable[0], usable[0]
	for _, iv := range usable[1:] {
		if rateOf(iv) > rateOf(peak) {
			peak = iv
		}
		if rateOf(iv) < rateOf(low) {
			low = iv
		}
	}
	return peak, low, true
}

func rateOf(iv Interval) float64 {
	span := iv.End - iv.Start
	if span <= 0 {
		return 0
	}
	return iv.Delta / span
}

// RouteKind classifies a request route: verify, issue or other. The prefixes
// come from the service's routing table.
func RouteKind(route string) string {
	if hasPrefix(route, "/system/verify_token") {
		return "verify"
	}
	if hasPrefix(route, "/app/access_token") {
		return "issue"
	}
	return "other"
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// LEKey parses a histogram `le` label value. +Inf/Inf and unparseable values
// return ok=false: the Python skill counts those rows toward the total.
func LEKey(text string) (float64, bool) {
	t := strings.TrimSpace(text)
	if lt := strings.ToLower(t); lt == "+inf" || lt == "inf" {
		return 0, false
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Bucket is one cumulative histogram bucket (le, count) after differencing.
type Bucket struct {
	LE float64
	C  float64
}

// HistogramQuantile interpolates a quantile from cumulative diffed buckets,
// ported from the Python skill. ok=false when it cannot be computed.
func HistogramQuantile(q float64, buckets []Bucket, total float64) (float64, bool) {
	if total <= 0 || len(buckets) == 0 {
		return 0, false
	}
	rank := q * total
	prevLE, prevC := 0.0, 0.0
	for _, b := range buckets {
		if b.C >= rank {
			span := b.C - prevC
			if span <= 0 {
				return b.LE, true
			}
			return prevLE + (b.LE-prevLE)*(rank-prevC)/span, true
		}
		prevLE, prevC = b.LE, b.C
	}
	return buckets[len(buckets)-1].LE, true
}

// GaugeMax returns the maximum sample across all rows.
func GaugeMax(rows []Row) (float64, bool) {
	best, ok := 0.0, false
	for _, row := range rows {
		for _, s := range row.Values {
			if !ok || s.V > best {
				best, ok = s.V, true
			}
		}
	}
	return best, ok
}
