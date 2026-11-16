// Package inspect runs the PRO inspection vertical slice over the two
// backends: thanos for the Go service's own metrics, OpenObserve for mysql
// gauges and the service.stats log histogram. Judgement lines are ported from
// the Python skill; PRO thresholds are provisional (known-items #22).
package inspect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/luoxw/iacctl-go/internal/prom"
	"github.com/luoxw/iacctl-go/internal/runtimecfg"
	"github.com/luoxw/iacctl-go/internal/series"
	"github.com/luoxw/iacctl-go/packs"
)

const step = 300

// CST is the display timezone.
var CST = time.FixedZone("CST", 8*3600)

type Check struct {
	ID       string         `json:"id"`
	Layer    string         `json:"layer"`
	Status   string         `json:"status"` // green|yellow|red|unknown
	Message  string         `json:"message"`
	Evidence map[string]any `json:"evidence,omitempty"`
}

type WindowOut struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Label string    `json:"label"`
}

type Result struct {
	Skill       string          `json:"skill"`
	Env         string          `json:"env"`
	GeneratedAt time.Time       `json:"generated_at"`
	Window      WindowOut       `json:"window"`
	Overall     string          `json:"overall"`
	Checks      []Check         `json:"checks"`
	Gaps        []string        `json:"gaps"`
	Facts       map[string]any  `json:"-"`
	Queries     []prom.LogEntry `json:"-"`
}

var rank = map[string]int{"green": 0, "unknown": 1, "yellow": 2, "red": 3}

type builder struct {
	checks []Check
	facts  map[string]any
}

func (b *builder) add(id, layer, status, message string, evidence map[string]any) {
	b.checks = append(b.checks, Check{ID: id, Layer: layer, Status: status, Message: message, Evidence: evidence})
}

func (b *builder) missing(id, layer, message, query string) {
	b.add(id, layer, "unknown", message, map[string]any{"query": query, "value": nil, "unit": "", "threshold": "无数据不判绿"})
	b.facts["_gap_"+id] = message
}

type fetched struct {
	rows []series.Row
	err  error
}

// errText returns the error text, or the fallback when the fetch succeeded.
func (f fetched) errText(fallback string) string {
	if f.err != nil {
		return fmt.Sprintf("%T: %s", f.err, f.err.Error())
	}
	return fallback
}

func grab(ctx context.Context, c *prom.Client, expr string, start, end float64) fetched {
	rows, err := c.PromRange(ctx, expr, start, end, step)
	return fetched{rows, err}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T: %s", err, err.Error())
}

// Run executes the inspection. AuthError is returned as-is: the caller must
// exit non-zero without printing a report.
func Run(ctx context.Context, thanos, obs *prom.Client, res packs.Resources, start, end time.Time, windowLabel string) (*Result, error) {
	b := &builder{facts: map[string]any{}}
	svc := res.Resources.Service
	thSel := res.Selector.Thanos
	obSel := res.Selector.Obs

	t0, t1 := float64(start.UnixMilli())/1000.0, float64(end.UnixMilli())/1000.0
	now := time.Now()
	nowTs := float64(now.UnixMilli()) / 1000.0
	dur := t1 - t0
	counterEnd := series.QueryEndCovering(t1, step)

	// ---- L1: traffic and result codes (thanos, local diff) ----
	httpF := grab(ctx, thanos, fmt.Sprintf("sum by (route, result) (iac_http_requests_total{%s})", thSel), t0, counterEnd)
	admF := grab(ctx, thanos, fmt.Sprintf("sum(iac_admission_rejected_total{%s})", thSel), t0, counterEnd)

	byKind := map[string]map[string]float64{}
	verifyIntervals := map[[2]float64]float64{}
	verifyTotal, issueTotal := 0.0, 0.0
	var httpRows []series.Row
	tLo, tHi := 0.0, 0.0
	boundsOK := false
	covered := dur
	if httpF.err == nil {
		httpRows = series.DropAfter(httpF.rows, nowTs)
		if lo, hi, ok := series.CoverageBounds(httpRows, t0, t1); ok {
			tLo, tHi, boundsOK = lo, hi, true
			covered = series.CoveredSpan(lo, hi, dur)
			httpRows = series.ClipRows(httpRows, lo, hi)
		}
		b.facts["covered_s"] = covered
		b.facts["window_s"] = dur

		for _, row := range httpRows {
			kind := series.RouteKind(row.Metric["route"])
			result := row.Metric["result"]
			if byKind[kind] == nil {
				byKind[kind] = map[string]float64{}
			}
			for k, d := range series.DeltaMap([]series.Row{row}) {
				byKind[kind][result] += d
				if kind == "verify" {
					verifyIntervals[k] += d
				}
			}
		}
		sum := func(kind string) float64 {
			var s float64
			for _, v := range byKind[kind] {
				s += v
			}
			return s
		}
		verifyTotal, issueTotal = sum("verify"), sum("issue")

		if len(verifyIntervals) == 0 || verifyTotal <= 0 {
			b.missing("l1.qps", "L1", httpF.errText("窗口内校验计数没有增量"), "iac_http_requests_total")
			b.missing("l1.5xx", "L1", "没有校验增量，5xx 占比无法计算", "iac_http_requests_total")
			b.missing("l1.401", "L1", "没有校验增量，401 占比无法计算", "iac_http_requests_total")
		} else {
			var ivs []series.Interval
			for k, d := range verifyIntervals {
				ivs = append(ivs, series.Interval{Start: k[0], End: k[1], Delta: d})
			}
			sort.Slice(ivs, func(i, j int) bool { return ivs[i].Start < ivs[j].Start })
			peak, low, hasExtremes := series.RateExtremes(ivs, step, nowTs, true)
			mean := 0.0
			if covered > 0 {
				mean = verifyTotal / covered
			}
			extreme := "5 分钟峰谷没有被数据完整盖住的桶"
			if hasExtremes {
				b.facts["peak_qps"] = peak.Delta / (peak.End - peak.Start)
				b.facts["low_qps"] = low.Delta / (low.End - low.Start)
				extreme = fmt.Sprintf("5 分钟峰值 %.0f（%s），低点 %.0f（%s）",
					peak.Delta/(peak.End-peak.Start), fmtHM(peak.End), low.Delta/(low.End-low.Start), fmtHM(low.End))
			}
			b.facts["verify_total"] = verifyTotal
			b.facts["issue_total"] = issueTotal
			b.facts["mean_qps"] = mean
			b.add("l1.qps", "L1", "green",
				fmt.Sprintf("校验 %.0f 次，均值 %.0f qps，%s。签发 %.0f 次。覆盖 %s–%s（%.0f 秒，请求窗 %.0f 秒）",
					verifyTotal, mean, extreme, issueTotal, fmtDT(tLo), fmtDT(tHi), covered, dur),
				map[string]any{
					"query":     fmt.Sprintf("sum by (route,result) (iac_http_requests_total{%s}) 的原始计数差分 / 首尾样本时间差，step=300，查询结束多取一个 step", thSel),
					"value":     round2(mean),
					"unit":      "qps",
					"threshold": "信息项，不设红黄线",
				})

			// 5xx per route kind
			type kindRow struct {
				kind, label string
				total, n5   float64
			}
			var bits []string
			worst, worstRatio := "green", 0.0
			verify5xx, issue5xx, other5xx := byKind["verify"]["system_error"], byKind["issue"]["system_error"], byKind["other"]["system_error"]
			for _, kr := range []kindRow{
				{"verify", "校验", verifyTotal, verify5xx},
				{"issue", "签发", issueTotal, issue5xx},
				{"other", "其他", sum("other"), other5xx},
			} {
				if kr.total <= 0 && kr.n5 <= 0 {
					continue
				}
				ratio := 1.0
				if kr.total > 0 {
					ratio = kr.n5 / kr.total
				}
				worstRatio = maxF(worstRatio, ratio)
				st := "green"
				if ratio >= 0.01 {
					st = "red"
				} else if ratio >= 0.001 {
					st = "yellow"
				}
				worst = worse(worst, st)
				bits = append(bits, fmt.Sprintf("%s %.0f/%.0f（%.4f%%）", kr.label, kr.n5, kr.total, ratio*100))
			}
			b.facts["five_ratio"] = worstRatio
			b.facts["five_total"] = verify5xx + issue5xx + other5xx
			b.add("l1.5xx", "L1", worst, "5xx "+strings.Join(bits, "；"),
				map[string]any{
					"query":     "iac_http_requests_total{result=\"system_error\"} 差分 / 同路由全部结果",
					"value":     worstRatio,
					"unit":      "ratio",
					"threshold": "≥0.1% 黄，≥1% 红",
				})

			reject := byKind["verify"]["business_reject"]
			share := 0.0
			if verifyTotal > 0 {
				share = reject / verifyTotal
			}
			b.facts["auth_401"] = share
			b.add("l1.401", "L1", "green",
				fmt.Sprintf("校验 business_reject %.0f，占校验 %.4f%%", reject, share*100),
				map[string]any{
					"query":     "result=\"business_reject\" / 校验请求",
					"value":     share,
					"unit":      "ratio",
					"threshold": "信息项，不设红黄线",
				})
		}
	} else {
		b.missing("l1.qps", "L1", errText(httpF.err), "iac_http_requests_total")
		b.missing("l1.5xx", "L1", "查询失败，5xx 占比无法计算", "iac_http_requests_total")
		b.missing("l1.401", "L1", "查询失败，401 占比无法计算", "iac_http_requests_total")
	}

	// admission
	if admF.err != nil || len(admF.rows) == 0 {
		msg := errText(admF.err)
		if msg == "" {
			msg = "iac_admission_rejected_total 无序列"
		}
		b.missing("l1.admission", "L1", msg, "iac_admission_rejected_total")
	} else {
		admRows := series.DropAfter(admF.rows, nowTs)
		var adm float64
		for _, d := range series.DeltaMap(admRows) {
			adm += d
		}
		b.facts["admission"] = adm
		st := "green"
		if adm > 0 {
			st = "yellow"
		}
		b.add("l1.admission", "L1", st, fmt.Sprintf("准入拒绝 %.0f", adm),
			map[string]any{
				"query":     fmt.Sprintf("sum(iac_admission_rejected_total{%s}) 差分", thSel),
				"value":     round2(adm),
				"unit":      "count",
				"threshold": ">0 黄",
			})
	}

	// ---- L2: latency (thanos bucket diff, local histogram quantile) ----
	bucketF := grab(ctx, thanos, fmt.Sprintf(`sum by (le) (iac_http_request_duration_seconds_bucket{%s,route=~"/system/verify_token.*"})`, thSel), t0, counterEnd)
	if bucketF.err != nil {
		b.missing("l2.latency_a", "L2", errText(bucketF.err), "iac_http_request_duration_seconds_bucket")
		b.missing("l2.latency_b", "L2", "延迟直方图查询失败，占比无法计算", "iac_http_request_duration_seconds_bucket")
		b.missing("l2.p99", "L2", "延迟直方图查询失败", "histogram_quantile")
	} else {
		bucketRows := series.DropAfter(bucketF.rows, nowTs)
		if !boundsOK {
			if lo, hi, ok := series.CoverageBounds(bucketRows, t0, t1); ok {
				tLo, tHi, boundsOK = lo, hi, true
				covered = series.CoveredSpan(lo, hi, dur)
			}
		}
		if boundsOK {
			bucketRows = series.ClipRows(bucketRows, tLo, tHi)
		}
		cumul := map[float64]float64{}
		bucketTotal := 0.0
		for _, row := range bucketRows {
			d := 0.0
			for _, v := range series.DeltaMap([]series.Row{row}) {
				d += v
			}
			if le, ok := series.LEKey(row.Metric["le"]); ok {
				cumul[le] += d
			} else {
				bucketTotal += d // +Inf rows count toward the total
			}
		}
		var buckets []series.Bucket
		for le, c := range cumul {
			buckets = append(buckets, series.Bucket{LE: le, C: c})
		}
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].LE < buckets[j].LE })
		if len(buckets) > 0 && bucketTotal <= 0 {
			bucketTotal = buckets[len(buckets)-1].C
		}
		if bucketTotal <= 0 {
			b.missing("l2.latency_a", "L2", "直方图在窗口内没有增量", "bucket")
			b.missing("l2.latency_b", "L2", "直方图在窗口内没有增量", "bucket")
			b.missing("l2.p99", "L2", "直方图在窗口内没有增量", "bucket")
		} else {
			la := cumul[0.025] / bucketTotal
			lb := cumul[0.1] / bucketTotal
			p99, _ := series.HistogramQuantile(0.99, buckets, bucketTotal)
			p999, _ := series.HistogramQuantile(0.999, buckets, bucketTotal)
			b.facts["lat_a"], b.facts["lat_b"] = la, lb
			b.facts["p99_ms"], b.facts["p999_ms"] = p99*1000, p999*1000
			stA := "green"
			if la < 0.99 {
				stA = "yellow"
			}
			b.add("l2.latency_a", "L2", stA, fmt.Sprintf("校验 ≤25ms %.4f%%", la*100),
				map[string]any{"query": `bucket le="0.025" 差分 / +Inf 差分，route=~"/system/verify_token.*"`, "value": fmt.Sprintf("%.4f", la*100), "unit": "percent", "threshold": "<99% 黄"})
			stB := "green"
			if lb < 0.995 {
				stB = "yellow"
			}
			b.add("l2.latency_b", "L2", stB, fmt.Sprintf("校验 ≤100ms %.4f%%", lb*100),
				map[string]any{"query": `bucket le="0.1" 差分 / +Inf 差分`, "value": fmt.Sprintf("%.4f", lb*100), "unit": "percent", "threshold": "<99.5% 黄"})
			b.add("l2.p99", "L2", "green",
				fmt.Sprintf("校验 p99 %.2f ms，p99.9 %.2f ms（由桶差分本地插值）", p99*1000, p999*1000),
				map[string]any{"query": "sum by (le) bucket 差分后做 histogram quantile", "value": fmt.Sprintf("%.3f", p99*1000), "unit": "ms", "threshold": "信息项；红黄线在延迟 A/B 占比上"})
		}
	}

	// ---- L3: redis timeouts (thanos diff; PRO thresholds provisional) ----
	toF := grab(ctx, thanos, fmt.Sprintf(`sum by (operation) (iac_dependency_errors_total{%s,dependency="redis",error_class="timeout"})`, thSel), t0, counterEnd)
	if toF.err != nil || len(toF.rows) == 0 {
		msg := errText(toF.err)
		if msg == "" {
			msg = "Redis 超时计数无序列"
		}
		b.missing("l3.redis_timeout", "L3", msg, "iac_dependency_errors_total")
	} else {
		toRows := series.DropAfter(toF.rows, nowTs)
		if boundsOK {
			toRows = series.ClipRows(toRows, tLo, tHi)
		}
		toByOp := map[string]float64{}
		toMap := map[[2]float64]float64{}
		for _, row := range toRows {
			op := row.Metric["operation"]
			if op == "" {
				op = "?"
			}
			for k, d := range series.DeltaMap([]series.Row{row}) {
				toByOp[op] += d
				toMap[k] += d
			}
		}
		toTotal := 0.0
		for _, v := range toByOp {
			toTotal += v
		}
		ratio, hasRatio := 0.0, false
		max5m, max5mAt := 0.0, 0.0
		if verifyTotal > 0 {
			ratio, hasRatio = toTotal/verifyTotal, true
			for k, d := range toMap {
				if v := verifyIntervals[k]; v > 0 && d/v > max5m {
					max5m = d / v
					max5mAt = k[1]
				}
			}
		}
		var ops []string
		for op, v := range toByOp {
			ops = append(ops, fmt.Sprintf("%s %.0f", op, v))
		}
		sort.Strings(ops)
		st := "green"
		if !hasRatio {
			st = "unknown"
		} else if ratio > 0.0005 || max5m >= 0.002 {
			st = "yellow"
		}
		b.facts["redis_to"] = toTotal
		if hasRatio {
			b.facts["redis_ratio"] = ratio
		}
		ratioTxt := "未知"
		if hasRatio {
			ratioTxt = fmt.Sprintf("%.4f‰", ratio*1000)
		}
		when := "?"
		if max5mAt > 0 {
			when = fmtHM(max5mAt)
		}
		b.add("l3.redis_timeout", "L3", st,
			fmt.Sprintf("Redis 超时 %.0f（%s），占校验 %s；5 分钟最高占比 %.3f‰（%s）",
				toTotal, strings.Join(ops, "，"), ratioTxt, max5m*1000, when),
			map[string]any{"query": `iac_dependency_errors_total{dependency="redis",error_class="timeout"} 差分 / 校验请求`, "value": fmt.Sprintf("%.4f", ratio*1000), "unit": "per_mille", "threshold": "窗口 >0.5‰ 或任一 5 分钟 ≥2‰ 为黄（临时线）；计数本身不是故障"})
	}

	// ---- L3: mysql fallback / cache untrusted ----
	fbF := grab(ctx, thanos, fmt.Sprintf("sum(iac_verify_db_fallback_total{%s})", thSel), t0, counterEnd)
	fbrF := grab(ctx, thanos, fmt.Sprintf("sum(iac_verify_db_fallback_rejected_total{%s})", thSel), t0, counterEnd)
	skipF := grab(ctx, thanos, fmt.Sprintf("sum(iac_verify_fallback_skipped_overload_total{%s})", thSel), t0, counterEnd)
	if fbF.err != nil || len(fbF.rows) == 0 || fbrF.err != nil || len(fbrF.rows) == 0 {
		msg := errText(fbF.err)
		if msg == "" && len(fbF.rows) == 0 {
			msg = "兜底计数无序列"
		}
		if msg == "" {
			msg = errText(fbrF.err)
		}
		if msg == "" {
			msg = "兜底拒绝无序列"
		}
		b.missing("l3.mysql_fallback", "L3", msg, "iac_verify_db_fallback_total")
	} else {
		sumD := func(f fetched) float64 {
			var t float64
			for _, v := range series.DeltaMap(series.DropAfter(f.rows, nowTs)) {
				t += v
			}
			return t
		}
		fb := sumD(fbF)
		fbr := sumD(fbrF)
		skipTxt := "过载跳过兜底无序列"
		if skipF.err == nil {
			skipTxt = fmt.Sprintf("过载跳过兜底 %.0f（信息，不升黄：在途 ≥ 0.9×校验并发上限时，已建立连接上的读超时不进 MySQL）", sumD(skipF))
			b.facts["fallback_skipped_overload"] = sumD(skipF)
		}
		b.facts["fallback"], b.facts["fallback_rejected"] = fb, fbr
		st := "green"
		if fbr > 0 {
			st = "yellow"
		}
		b.add("l3.mysql_fallback", "L3", st,
			fmt.Sprintf("MySQL 兜底 %.0f，兜底被拒 %.0f。%s", fb, fbr, skipTxt),
			map[string]any{"query": "iac_verify_db_fallback_total / _rejected_total / _skipped_overload_total 差分", "value": fbr, "unit": "count", "threshold": "被拒 >0 黄；兜底次数本身是超时后的正常退路"})
	}
	cacheF := grab(ctx, thanos, fmt.Sprintf("max(iac_cache_untrusted{%s})", thSel), t0, t1)
	if cacheF.err != nil {
		b.missing("l3.cache_untrusted", "L3", errText(cacheF.err), "iac_cache_untrusted")
	} else if mx, ok := series.GaugeMax(series.DropAfter(cacheF.rows, nowTs)); !ok {
		b.missing("l3.cache_untrusted", "L3", "iac_cache_untrusted 无样本", "iac_cache_untrusted")
	} else {
		st := "green"
		if mx > 0 {
			st = "yellow"
		}
		b.facts["cache_untrusted"] = mx
		b.add("l3.cache_untrusted", "L3", st, fmt.Sprintf("iac_cache_untrusted 窗口最大 %.0f", mx),
			map[string]any{"query": "max(iac_cache_untrusted)", "value": mx, "unit": "gauge", "threshold": ">0 黄"})
	}

	// ---- L0: sample age (thanos instant) ----
	freshRows, freshErr := thanos.Prom(ctx, fmt.Sprintf("iac_http_requests_total{%s}", thSel), nowTs)
	if freshErr != nil {
		b.missing("l0.sample_age", "L0", errText(freshErr), "iac_http_requests_total")
	} else if len(freshRows) == 0 {
		b.missing("l0.sample_age", "L0", "没有取到 iac_http_requests_total 的最新样本", "iac_http_requests_total")
	} else {
		newest := 0.0
		for _, row := range freshRows {
			if len(row.Values) > 0 && row.Values[0].T > newest {
				newest = row.Values[0].T
			}
		}
		if newest <= 0 {
			b.missing("l0.sample_age", "L0", "最新样本没有时间戳", "iac_http_requests_total")
		} else {
			age := nowTs - newest
			st := "green"
			if age > 180 && t1 >= nowTs-120 {
				st = "yellow"
			}
			b.facts["sample_age_s"] = age
			b.add("l0.sample_age", "L0", st,
				fmt.Sprintf("iac_http_requests_total 最新样本距现在 %.0f 秒", age),
				map[string]any{
					"query":     fmt.Sprintf("iac_http_requests_total{%s}", thSel),
					"value":     round1(age),
					"unit":      "seconds",
					"threshold": "活窗口内最新样本超过 180 秒为黄",
				})
		}
	}

	// ---- L0: service.stats continuity (O2 SQL) ----
	statsGapCheck(ctx, b, obs, svc, obSel, res.Resources.LogStream, start, end)

	// ---- L3: mysql running (O2 gauge) ----
	mysqlF := grab(ctx, obs, fmt.Sprintf("max(mysql_threads{service_name=%q,kind=\"running\"})", res.Resources.MysqlService), t0, t1)
	if mysqlF.err != nil {
		b.missing("l3.mysql_running", "L3", errText(mysqlF.err), "mysql_threads")
	} else if len(mysqlF.rows) == 0 {
		b.missing("l3.mysql_running", "L3", "mysql_threads 无序列", "mysql_threads")
	} else {
		var pts []float64
		for _, row := range mysqlF.rows {
			for _, s := range row.Values {
				pts = append(pts, s.V)
			}
		}
		if len(pts) == 0 {
			b.missing("l3.mysql_running", "L3", "mysql_threads 无样本", "mysql_threads")
		} else {
			mx := pts[0]
			for _, v := range pts[1:] {
				mx = maxF(mx, v)
			}
			st := "green"
			if mx >= 20 {
				st = "red"
			}
			b.facts["mysql_max"] = mx
			b.add("l3.mysql_running", "L3", st, fmt.Sprintf("MySQL Threads_running 最大 %.0f（%s）", mx, res.Resources.MysqlService),
				map[string]any{
					"query":     fmt.Sprintf("max(mysql_threads{service_name=%q,kind=\"running\"})", res.Resources.MysqlService),
					"value":     mx,
					"unit":      "threads",
					"threshold": "≥20 红（临时线，known-items 第 22 条）",
				})
		}
	}

	// ---- L4: pods snapshot (ccloud-cli; thanos kube-state fallback for
	// sandboxes without ccloud-cli, e.g. the OMA runtime), cpu, version ----
	expect := res.Resources.ExpectReplicas
	var ready, total, restarts int
	var l4Src, l4Detail string
	pods, podErr := ccloudPods(res.Resources.IacApp, res.Resources.ClusterID)
	if podErr == nil {
		running := 0
		for _, p := range pods {
			if p.Phase == "Running" {
				running++
				if p.Ready {
					ready++
				}
			}
			restarts += p.Restarts
		}
		total = len(pods)
		l4Src = fmt.Sprintf("ccloud-cli app pods --app-id %s --cluster-id %s", res.Resources.IacApp, res.Resources.ClusterID)
		var rs []string
		for _, p := range pods {
			if p.Restarts > 0 {
				rs = append(rs, fmt.Sprintf("%s=%d@%s", shortInst(p.Name), p.Restarts, p.Node))
			}
		}
		if len(rs) > 0 {
			l4Detail = strings.Join(rs, "；")
		}
		b.facts["iac_running"] = running
	} else {
		// kube-state fallback via thanos (verified against ccloud-cli on 2026-11-11)
		kubeSel := fmt.Sprintf("namespace=%q,pod=~%q", res.Resources.Namespace, "app-"+res.Resources.IacApp+".*")
		readyRows, e1 := thanos.Prom(ctx, fmt.Sprintf("count(kube_pod_status_ready{%s,condition=\"true\"})", kubeSel), nowTs)
		totalRows, e2 := thanos.Prom(ctx, fmt.Sprintf("count(kube_pod_info{%s})", kubeSel), nowTs)
		rstRows, e3 := thanos.Prom(ctx, fmt.Sprintf("sum(max by (pod) (kube_pod_container_status_restarts_total{%s}))", kubeSel), nowTs)
		if e1 != nil || e2 != nil || e3 != nil || len(readyRows) == 0 || len(totalRows) == 0 || len(rstRows) == 0 {
			msg := errText(e1)
			if msg == "" {
				msg = errText(e2)
			}
			if msg == "" {
				msg = errText(e3)
			}
			if msg == "" {
				msg = "kube-state 指标无序列"
			}
			b.missing("l4.replicas", "L4", "ccloud-cli 与 kube-state 均不可用："+msg, "ccloud-cli app pods / kube_pod_*")
			b.missing("l4.restarts", "L4", "ccloud-cli 与 kube-state 均不可用", "ccloud-cli app pods / kube_pod_*")
			ready = -1
		} else {
			ready = int(readyRows[0].Values[0].V)
			total = int(totalRows[0].Values[0].V)
			restarts = int(rstRows[0].Values[0].V)
			l4Src = "thanos kube-state 回退（无 ccloud-cli）：count(kube_pod_status_ready/pod_info/container_status_restarts_total)"
		}
	}
	if ready >= 0 {
		st := "green"
		if ready < expect {
			st = "red"
		}
		b.facts["iac_ready"], b.facts["iac_total"], b.facts["expect"], b.facts["iac_restarts"] = ready, total, expect, restarts
		b.add("l4.replicas", "L4", st,
			fmt.Sprintf("iac-pro 当前 ready %d/%d（列表 %d）。这是当前快照，不随历史窗口变化", ready, expect, total),
			map[string]any{"query": l4Src, "value": ready, "unit": "pods", "threshold": fmt.Sprintf("ready < %d 红", expect)})
		stR := "green"
		if restarts > 0 {
			stR = "yellow"
		}
		msg := "全部为 0"
		if l4Detail != "" {
			msg = l4Detail
		} else if restarts > 0 {
			msg = fmt.Sprintf("合计 %d（kube-state 回退，无逐 Pod 明细）", restarts)
		}
		b.add("l4.restarts", "L4", stR, "重启次数 "+msg,
			map[string]any{"query": l4Src, "value": restarts, "unit": "restarts", "threshold": ">0 黄"})
	}
	rateStart := t0 + step
	if dur < step {
		rateStart = t0
	}
	cpuF := grab(ctx, thanos, fmt.Sprintf("sum by (pod_name) (rate(process_cpu_seconds_total{%s}[5m]))", thSel), rateStart, t1)
	if cpuF.err != nil {
		b.missing("l4.cpu", "L4", errText(cpuF.err), "process_cpu_seconds_total")
	} else {
		bestV, bestPod, ok := 0.0, "", false
		for _, row := range series.DropAfter(cpuF.rows, nowTs) {
			if mx, ok2 := series.GaugeMax([]series.Row{row}); ok2 && (!ok || mx > bestV) {
				bestV, bestPod, ok = mx, row.Metric["pod_name"], true
			}
		}
		if !ok {
			b.missing("l4.cpu", "L4", "CPU 速率无样本", "process_cpu_seconds_total")
		} else {
			b.facts["pod_cpu"] = bestV
			b.add("l4.cpu", "L4", "green",
				fmt.Sprintf("单 Pod CPU 最大 %.3f 核（%s）", bestV, shortInst(bestPod)),
				map[string]any{"query": fmt.Sprintf("sum by (pod_name) (rate(process_cpu_seconds_total{%s}[5m]))", thSel), "value": fmt.Sprintf("%.3f", bestV), "unit": "cores", "threshold": "信息项；本巡检不把 Pod CPU 单独升黄"})
		}
	}
	verF := grab(ctx, thanos, fmt.Sprintf("sum by (app_version_num) (iac_http_requests_total{%s})", thSel), t0, counterEnd)
	if verF.err != nil {
		b.missing("l4.version", "L4", errText(verF.err), "app_version_num")
	} else {
		type verRow struct {
			d   float64
			ver string
		}
		var versions []verRow
		for _, row := range series.DropAfter(verF.rows, nowTs) {
			d := 0.0
			for _, v := range series.DeltaMap([]series.Row{row}) {
				d += v
			}
			if d > 0 {
				ver := row.Metric["app_version_num"]
				if ver == "" {
					ver = "?"
				}
				versions = append(versions, verRow{d, ver})
			}
		}
		sort.SliceStable(versions, func(i, j int) bool { return versions[i].d > versions[j].d })
		if len(versions) == 0 {
			b.missing("l4.version", "L4", "窗口内没有带版本的请求增量", "app_version_num")
		} else {
			var shown, vs []string
			for i, v := range versions {
				if i < 6 {
					shown = append(shown, fmt.Sprintf("%s %.0f", v.ver, v.d))
				}
				vs = append(vs, v.ver)
			}
			b.facts["versions"] = vs
			b.add("l4.version", "L4", "green",
				"窗内有流量的版本（thanos app_version_num）："+strings.Join(shown, "，"),
				map[string]any{"query": fmt.Sprintf("sum by (app_version_num) (iac_http_requests_total{%s}) 差分；O2 侧对应 service_version", thSel), "value": len(versions), "unit": "versions", "threshold": "信息项"})
		}
	}

	// ---- L7: 5xx log details (O2 SQL; only when metric 5xx > 0) ----
	if five, ok := b.facts["five_total"].(float64); ok && five > 0 {
		sql := fmt.Sprintf(
			"SELECT route, iac_err_code, iac_path, iac_redis_error, count(*) AS n, min(_timestamp) AS first_us, max(_timestamp) AS last_us FROM %q "+
				"WHERE service_name = '%s' AND %s AND body = 'http.request' AND iac_err_code >= 50000 "+
				"GROUP BY route, iac_err_code, iac_path, iac_redis_error ORDER BY n DESC",
			res.Resources.LogStream, svc, obSel2sql(obSel))
		out, err := obs.SearchSQL(ctx, "logs", sql, t0, t1, 100)
		switch {
		case err != nil:
			b.missing("l7.5xx", "L7", errText(err), "5xx 日志聚合")
		case boolVal(out["is_partial"]):
			b.missing("l7.5xx", "L7", "5xx 日志聚合不完整（is_partial）", sql)
		default:
			type grp struct {
				Route, Code, Path, RedisErr string
				N                           float64
				First, Last                 string
			}
			var groups []grp
			for _, h := range listVal(out["hits"]) {
				row, ok := h.(map[string]any)
				if !ok {
					continue
				}
				groups = append(groups, grp{
					Route:    strVal(row["route"]),
					Code:     strVal(row["iac_err_code"]),
					Path:     strVal(row["iac_path"]),
					RedisErr: strVal(row["iac_redis_error"]),
					N:        numVal(row["n"]),
					First:    usToCST(row["first_us"]),
					Last:     usToCST(row["last_us"]),
				})
			}
			if len(groups) == 0 {
				b.add("l7.5xx", "L7", "unknown",
					fmt.Sprintf("指标里 5xx 为 %.0f，日志聚合是 0 行。日志流有采样，0 行不等于没有 5xx", five),
					map[string]any{"query": sql, "value": 0, "unit": "rows", "threshold": "有指标 5xx 但日志为空则不判绿"})
			} else {
				var bits []string
				for i, g := range groups {
					if i >= 8 {
						break
					}
					bits = append(bits, fmt.Sprintf("%s/%s/%s/%s×%.0f", g.Code, g.Route, g.Path, g.RedisErr, g.N))
				}
				b.facts["five_groups"] = strings.Join(bits, "；")
				skipTxt := "过载跳过兜底无序列，不能用它解释 iac_path=other。"
				if sv, ok := b.facts["fallback_skipped_overload"].(float64); ok {
					skipTxt = fmt.Sprintf("过载跳过兜底 %.0f（设计行为：在途 ≥ 0.9×校验并发上限时跳过 MySQL；iac_path=other 是空 path 默认值）", sv)
				}
				b.add("l7.5xx", "L7", "green",
					"日志分组（有采样，条数不必等于指标）："+strings.Join(bits, "；")+"。"+skipTxt,
					map[string]any{"query": sql, "value": len(groups), "unit": "groups", "threshold": "信息项；分组用于定位"})
				b.facts["five_groups_detail"] = groups
			}
		}
	}

	// ---- L8: stream sizes (O2 streams API; informational) ----
	{
		var streams []map[string]any
		for _, typ := range []string{"logs", "traces"} {
			name := res.Resources.LogStream
			if typ == "traces" {
				name = res.Resources.TraceStream
			}
			out, err := obs.GetJSON(ctx, "/api/"+runtimecfg.Current.Obs.Org+"/streams",
				url.Values{"type": {typ}, "fetchSchema": {"false"}, "keyword": {name}})
			if err != nil {
				continue
			}
			for _, item := range listVal(out["list"]) {
				m, ok := item.(map[string]any)
				if !ok || strVal(m["name"]) != name {
					continue
				}
				st, _ := m["stats"].(map[string]any)
				streams = append(streams, map[string]any{
					"name": name, "type": typ,
					"doc_num": st["doc_num"], "storage_size": st["storage_size"], "doc_time_max": st["doc_time_max"],
				})
			}
		}
		if len(streams) < 2 {
			b.missing("l8.streams", "L8", "没有同时拿到日志流和 trace 流的统计", "GET streams")
		} else {
			var bits []string
			for _, s := range streams {
				bits = append(bits, fmt.Sprintf("%v:%v doc %v size %v", s["type"], s["name"], s["doc_num"], s["storage_size"]))
			}
			b.add("l8.streams", "L8", "green", "流存量（信息；保留策略会裁，不做差值）："+strings.Join(bits, "；"),
				map[string]any{"query": "GET /api/{org}/streams?type=logs|traces&fetchSchema=false", "value": len(streams), "unit": "streams", "threshold": "信息项"})
			b.facts["streams"] = streams
		}
	}

	overall := "green"
	for _, c := range b.checks {
		if rank[c.Status] > rank[overall] {
			overall = c.Status
		}
	}
	var gaps []string
	for _, c := range b.checks {
		if c.Status == "unknown" {
			gaps = append(gaps, c.ID+": "+c.Message)
		}
	}
	queries := append(append([]prom.LogEntry{}, thanos.Log...), obs.Log...)
	return &Result{
		Skill:       "iacctl-go",
		Env:         res.Env,
		GeneratedAt: now,
		Window:      WindowOut{Start: start, End: end, Label: windowLabel},
		Overall:     overall,
		Checks:      b.checks,
		Gaps:        gaps,
		Facts:       b.facts,
		Queries:     queries,
	}, nil
}

// podInfo is one entry of `ccloud-cli app pods`.
type podInfo struct {
	Name     string `json:"podName"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Restarts int    `json:"restartCount"`
	Node     string `json:"nodeName"`
	IP       string `json:"podIP"`
	Start    string `json:"startTime"`
}

// ccloudPods runs the read-only `ccloud-cli app pods` snapshot, mirroring the
// Python skill: parse the first JSON object from stdout regardless of exit
// code, and require data to be a pod list.
func ccloudPods(appID, clusterID string) ([]podInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ccloud-cli", "app", "pods", "--app-id", appID, "--cluster-id", clusterID)
	out, _ := cmd.Output()
	text := string(out)
	i := strings.Index(text, "{")
	if i < 0 {
		return nil, fmt.Errorf("ccloud 调用失败或没有返回 JSON（ccloud-cli 需在 PATH）")
	}
	var obj struct {
		Data []podInfo `json:"data"`
	}
	if err := json.Unmarshal([]byte(text[i:]), &obj); err != nil {
		return nil, fmt.Errorf("ccloud JSON 无法解析：%v", err)
	}
	if obj.Data == nil {
		return nil, fmt.Errorf("ccloud data 不是 Pod 列表")
	}
	return obj.Data, nil
}

// statsGapCheck ports the L0 service.stats continuity check: every pod should
// log service.stats once per minute; ≥2 consecutive missing minutes is yellow.
// The window is queried in 30-minute slices because OpenObserve's histogram
// sometimes returns only the first few hundred minutes in one shot.
func statsGapCheck(ctx context.Context, b *builder, obs *prom.Client, svc, obSel, stream string, start, end time.Time) {
	perInstance := map[string]map[string]bool{}
	unknown := ""
	for sliceStart := start; sliceStart.Before(end); sliceStart = sliceStart.Add(30 * time.Minute) {
		sliceEnd := sliceStart.Add(30 * time.Minute)
		if sliceEnd.After(end) {
			sliceEnd = end
		}
		sql := fmt.Sprintf(
			"SELECT histogram(_timestamp, '1 minute') AS m, service_instance_id, count(*) AS c FROM %q "+
				"WHERE service_name = '%s' AND %s AND body = 'service.stats' "+
				"GROUP BY m, service_instance_id ORDER BY m",
			stream, svc, obSel2sql(obSel))
		out, err := obs.SearchSQL(ctx, "logs", sql, float64(sliceStart.UnixMilli())/1000.0, float64(sliceEnd.UnixMilli())/1000.0, 10000)
		if err != nil {
			unknown = errText(err)
			break
		}
		if v, ok := out["is_partial"].(bool); ok && v {
			unknown = "切片返回 is_partial"
			break
		}
		hits, _ := out["hits"].([]any)
		total := -1.0
		if tv, ok := out["total"].(float64); ok {
			total = tv
		}
		if total > float64(len(hits)) {
			unknown = fmt.Sprintf("切片 total=%v 超过返回行数 %d", total, len(hits))
			break
		}
		for _, h := range hits {
			row, ok := h.(map[string]any)
			if !ok {
				continue
			}
			m, _ := row["m"].(string)
			inst, _ := row["service_instance_id"].(string)
			if m == "" || inst == "" {
				continue
			}
			if perInstance[inst] == nil {
				perInstance[inst] = map[string]bool{}
			}
			perInstance[inst][m] = true
		}
	}
	if unknown != "" {
		b.missing("l0.stats_gap", "L0", "service.stats 查询失败："+unknown, "service.stats")
		return
	}
	if len(perInstance) == 0 {
		b.missing("l0.stats_gap", "L0", "窗口内没有任何 service.stats", "service.stats")
		return
	}
	// per instance: minutes present from its first minute to window end
	maxGap, gapInst := 0, ""
	for inst, minutes := range perInstance {
		var keys []string
		for k := range minutes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		gap := 0
		for i := 1; i < len(keys); i++ {
			prev, _ := time.ParseInLocation("2006-01-02T15:04:05", keys[i-1], CST)
			cur, _ := time.ParseInLocation("2006-01-02T15:04:05", keys[i], CST)
			missing := int(cur.Sub(prev).Minutes()) - 1
			if missing > gap {
				gap = missing
			}
		}
		if gap > maxGap {
			maxGap, gapInst = gap, inst
		}
	}
	st := "green"
	if maxGap >= 2 {
		st = "yellow"
	}
	b.facts["stats_instances"] = len(perInstance)
	b.facts["stats_gap_minutes"] = maxGap
	b.add("l0.stats_gap", "L0", st,
		fmt.Sprintf("service.stats 涉及 %d 个实例，最长连续缺分钟 %d（%s）。%s",
			len(perInstance), maxGap, shortInst(gapInst), map[bool]string{true: "有连续 2 分钟以上的空档", false: "没有连续 2 分钟的空档"}[maxGap >= 2]),
		map[string]any{
			"query":     "SELECT histogram(_timestamp,'1 minute') ... GROUP BY m, service_instance_id（30 分钟切片）",
			"value":     maxGap,
			"unit":      "minutes",
			"threshold": "同一实例连续 ≥2 分钟没有 service.stats 为黄",
		})
}

// obSel2sql converts the metrics selector (k="v") to an SQL AND clause.
// Metrics syntax double-quotes values; SQL string literals need single quotes.
func obSel2sql(sel string) string {
	var parts []string
	for _, kv := range strings.Split(sel, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		parts = append(parts, fmt.Sprintf("%s = '%s'", strings.TrimSpace(k), v))
	}
	return strings.Join(parts, " AND ")
}

func boolVal(v any) bool { b, _ := v.(bool); return b }

func listVal(v any) []any { l, _ := v.([]any); return l }

func strVal(v any) string { s, _ := v.(string); return s }

func numVal(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		var f float64
		fmt.Sscanf(x, "%g", &f)
		return f
	}
	return 0
}

// usToCST renders an OpenObserve microsecond timestamp in CST.
func usToCST(v any) string {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return ""
	}
	return time.UnixMicro(int64(f)).In(CST).Format("2006-01-02T15:04:05")
}

func shortInst(inst string) string {
	if len(inst) > 24 {
		return inst[:12] + "…" + inst[len(inst)-8:]
	}
	if inst == "" {
		return "无缺口"
	}
	return inst
}

func worse(a, b string) string {
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func round1(v float64) any { return fmt.Sprintf("%.1f", v) }
func round2(v float64) any { return fmt.Sprintf("%.2f", v) }

func fmtDT(t float64) string {
	if t == 0 {
		return "?"
	}
	return time.Unix(int64(t), 0).In(CST).Format("2006-01-02 15:04:05")
}

func fmtHM(t float64) string {
	return time.Unix(int64(t), 0).In(CST).Format("15:04")
}

// RenderSummary prints the short text report.
func (r *Result) RenderSummary() string {
	f := r.Facts
	var lines []string
	lines = append(lines, fmt.Sprintf("## IAC-Go 巡检（iacctl-go）· %s CST · 窗口 %s · 环境 %s · 总体 %s",
		r.GeneratedAt.In(CST).Format("2006-01-02 15:04:05"), r.Window.Label, r.Env, levelName(r.Overall)))
	if r, ok := f["iac_ready"].(int); ok {
		exp, _ := f["expect"].(int)
		line := fmt.Sprintf("- 资源面：ready %d/%d · 重启 %d", r, exp, f["iac_restarts"])
		if v, ok2 := f["versions"].([]string); ok2 && len(v) > 0 {
			line += " · 版本 " + strings.Join(v, ",")
		}
		lines = append(lines, line)
	}
	if la, ok := f["lat_a"].(float64); ok {
		lines = append(lines, fmt.Sprintf("- 延迟：A %.4f%% · B %.4f%% · p99 %.2fms", la*100, f["lat_b"].(float64)*100, f["p99_ms"].(float64)))
	}
	if v, ok := f["verify_total"].(float64); ok {
		peakTxt := "峰谷无完整桶"
		if _, has := f["peak_qps"]; has {
			peakTxt = fmt.Sprintf("峰值 %.0f", f["peak_qps"])
		}
		lines = append(lines, fmt.Sprintf("- 流量：校验 %.0f 次 · 均值 %.0f qps · %s · 签发 %.0f 次",
			v, f["mean_qps"].(float64), peakTxt, f["issue_total"].(float64)))
	} else {
		lines = append(lines, "- 流量：未知")
	}
	if fr, ok := f["five_ratio"].(float64); ok {
		lines = append(lines, fmt.Sprintf("- 质量：5xx 最差路由占比 %.4f%%", fr*100))
	} else {
		lines = append(lines, "- 质量：未知")
	}
	var depBits []string
	if rt, ok := f["redis_to"].(float64); ok {
		bit := fmt.Sprintf("Redis 超时 %.0f", rt)
		if rr, ok2 := f["redis_ratio"].(float64); ok2 {
			bit += fmt.Sprintf("（%.4f‰）", rr*1000)
		}
		depBits = append(depBits, bit)
	}
	if fbv, ok := f["fallback"].(float64); ok {
		depBits = append(depBits, fmt.Sprintf("兜底 %.0f/拒 %.0f", fbv, f["fallback_rejected"].(float64)))
	}
	if mv, ok := f["mysql_max"].(float64); ok {
		depBits = append(depBits, fmt.Sprintf("MySQL running %.0f", mv))
	}
	if len(depBits) > 0 {
		lines = append(lines, "- 依赖："+strings.Join(depBits, " · "))
	} else {
		lines = append(lines, "- 依赖：未知")
	}
	if sa, ok := f["sample_age_s"].(float64); ok {
		if si, ok2 := f["stats_instances"].(int); ok2 {
			lines = append(lines, fmt.Sprintf("- 新鲜度：最新样本 %.0f 秒前 · stats 实例 %d", sa, si))
		} else {
			lines = append(lines, fmt.Sprintf("- 新鲜度：最新样本 %.0f 秒前 · stats 未知", sa))
		}
	}
	bad := 0
	for _, c := range r.Checks {
		if c.Status == "yellow" || c.Status == "red" {
			bad++
		}
	}
	gapN := 0
	for _, c := range r.Checks {
		if c.Status == "unknown" {
			gapN++
		}
	}
	lines = append(lines, fmt.Sprintf("- 异常 %d 项 · 缺口 %d 项（LP/L5/L6 由 Python skill 覆盖；阈值临时线见 known-items #2）", bad, gapN))
	return strings.Join(lines, "\n")
}

func levelName(s string) string {
	switch s {
	case "red":
		return "红"
	case "yellow":
		return "黄"
	case "unknown":
		return "未知"
	default:
		return "绿"
	}
}
