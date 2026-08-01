// Package e2e（本文件）：任务 11「降噪」两场景 E2E——R1/R2 端到端判据：
//   - 场景①：路由 group_by=["__template__"]，3 条只差 IP 的告警（相同 alertname，
//     不同 instance 标签保证 domain.Fingerprint 不同、是 3 条独立告警）应按标题模板
//     归一化后的指纹归入同一组，只发出一条聚合通知。
//   - 场景②：group_mode="window"（不配置 group_by），不同 alertname/fingerprint 的
//     告警只要命中同一条路由就归入同一个固定窗口组，同样只发出一条聚合通知。
//
// 两场景都各自新建团队 + 专用路由（与既有场景同一手法，见 journey_test.go），
// 避免与种子团队/其他场景互相干扰；断言复用 waitFor + repost 直到路由缓存
// （routeCacheTTL≤10s）生效的既有手法，双锁定 mockLark 收到的 GroupKey/AlertIDs
// 精确集合 + Target（=本场景团队 ID），防止误判成其他团队/场景的通知。
// 风暴熔断（storm_threshold_per_min）与 flapping 按设计裁剪不在本文件覆盖范围。
package e2e

import (
	"net/http"
	"testing"
	"time"
)

// noisePayload 构造一条 kind=alertmanager 的 webhook 请求体，与 helpers_test.go 的
// alertmanagerPayload 结构完全一致，唯一区别是 title（summary 注解）由调用方显式
// 指定而非硬编码成 alertname——场景①需要"只差 IP"的标题才能验证模板归一化，
// alertmanagerPayload 做不到这一点，故在本文件内单独实现，不改动共享 helper。
func noisePayload(alertname, severity, status, title string, extraLabels map[string]string, startsAt time.Time) []byte {
	labels := map[string]string{"alertname": alertname, "severity": severity}
	for k, v := range extraLabels {
		labels[k] = v
	}
	body := map[string]any{
		"version":  "4",
		"status":   status,
		"receiver": "e2e",
		"alerts": []map[string]any{
			{
				"status":      status,
				"labels":      labels,
				"annotations": map[string]string{"summary": title},
				"startsAt":    startsAt.Format(time.RFC3339Nano),
				"endsAt":      "0001-01-01T00:00:00Z",
			},
		},
	}
	return mustJSON(body)
}

// sameIDSet 判断 got 与 want 是否是完全相同的 ID 集合（忽略顺序、不容忍重复或
// 缺失）——比"got 包含 want 的某个元素"更强，用于锁定"聚合通知里的 AlertIDs
// 恰好是这一批告警，不多不少"。
func sameIDSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	remaining := make(map[string]bool, len(want))
	for _, w := range want {
		remaining[w] = true
	}
	for _, g := range got {
		if !remaining[g] {
			return false
		}
		delete(remaining, g)
	}
	return len(remaining) == 0
}

// TestScenarioNoise1_TemplateFingerprintMerge 验证 R2：group_by=["__template__"]
// 时，标题只差 IP（"Ping timeout from 10.0.0.x"，IP 是 domain.TemplateFingerprint
// 承认的变量 token）的多条告警归并到同一组，只发一条聚合通知。三条告警的 instance
// 标签各不相同，保证 domain.Fingerprint（不依赖 Title）判定为三条独立告警，而不是
// 被去重 stage 合并成一条——这样才是在验证"分组"而非"去重"。
func TestScenarioNoise1_TemplateFingerprintMerge(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()

	teamResp := adminCreate(t, client, "/api/v1/admin/teams", map[string]any{
		"name": "e2e-noise-team1-" + suffix,
	}, http.StatusCreated)
	teamID, _ := teamResp["id"].(string)
	if teamID == "" {
		t.Fatalf("e2e: 创建团队响应缺少 id: %+v", teamResp)
	}

	alertname := "e2e-noise1-tmpl-" + suffix
	adminCreate(t, client, "/api/v1/admin/routes", map[string]any{
		"priority":                6,
		"matchers":                []map[string]string{{"name": "alertname", "op": "=", "value": alertname}},
		"target_team_id":          teamID,
		"group_by":                []string{"__template__"},
		"group_wait_seconds":      8,
		"group_interval_seconds":  1,
		"repeat_interval_seconds": 60,
	}, http.StatusCreated)

	instances := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	startsAt := time.Now()
	post := func() {
		for _, ip := range instances {
			if _, _, err := client.postIngest(seedResult.AlertmanagerIntegrationID, seedResult.AlertmanagerToken,
				noisePayload(alertname, "critical", "firing", "Ping timeout from "+ip, map[string]string{"instance": ip}, startsAt)); err != nil {
				t.Fatalf("e2e: ingest 请求失败: %v", err)
			}
		}
	}

	// 与 journey_test.go 同一手法：路由/分组缓存生效前重投是安全的（dedup 按
	// fingerprint 合并，不产生重复告警），waitFor 条件把"三条都路由到本团队且归入
	// 同一组"与"聚合通知已发出"合并成一个判据，避免只锁到"部分成员入组时那一条
	// 不完整的通知"就误判通过。
	waitFor(t, 20*time.Second, time.Second, func() bool {
		post()

		ids := make([]string, 0, len(instances))
		var groupKey string
		for _, ip := range instances {
			a, ok, err := findAlertByLabel(client, "instance", ip)
			if err != nil || !ok || a.TeamID != teamID || a.GroupKey == "" {
				return false
			}
			if groupKey == "" {
				groupKey = a.GroupKey
			} else if a.GroupKey != groupKey {
				return false
			}
			ids = append(ids, a.ID)
		}

		for _, s := range mockLark.Sent() {
			if s.Target == teamID && s.Msg.GroupKey == groupKey && sameIDSet(s.Msg.AlertIDs, ids) {
				return true
			}
		}
		return false
	}, "3 条只差 IP 的告警按 __template__ 归并为一组并发出一条聚合通知")
}

// TestScenarioNoise2_WindowModeMergesAllFingerprints 验证 R1：group_mode="window"
// 时，即使 alertname/fingerprint 完全不同的告警，只要命中同一条路由，也归入同一个
// 固定窗口组（不使用 group_by），只发一条聚合通知。
func TestScenarioNoise2_WindowModeMergesAllFingerprints(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()

	teamResp := adminCreate(t, client, "/api/v1/admin/teams", map[string]any{
		"name": "e2e-noise-team2-" + suffix,
	}, http.StatusCreated)
	teamID, _ := teamResp["id"].(string)
	if teamID == "" {
		t.Fatalf("e2e: 创建团队响应缺少 id: %+v", teamResp)
	}

	// window 模式与 group_by 互斥（internal/api/config.go validateRoute），group_by
	// 显式传空数组；matcher 按自定义标签而非 alertname 匹配，因为本场景两条告警的
	// alertname 本身就不同（这正是要验证的"窗口模式不看 fingerprint/labels 差异"）。
	scenarioTag := "e2e-noise2-window-" + suffix
	adminCreate(t, client, "/api/v1/admin/routes", map[string]any{
		"priority":                7,
		"matchers":                []map[string]string{{"name": "noise_scenario", "op": "=", "value": scenarioTag}},
		"target_team_id":          teamID,
		"group_mode":              "window",
		"group_by":                []string{},
		"group_wait_seconds":      8,
		"group_interval_seconds":  1,
		"repeat_interval_seconds": 60,
	}, http.StatusCreated)

	alertnameA := "e2e-noise2-a-" + suffix
	alertnameB := "e2e-noise2-b-" + suffix
	startsAt := time.Now()
	post := func() {
		if _, _, err := client.postIngest(seedResult.AlertmanagerIntegrationID, seedResult.AlertmanagerToken,
			alertmanagerPayload(alertnameA, "critical", "firing", startsAt, map[string]string{"noise_scenario": scenarioTag})); err != nil {
			t.Fatalf("e2e: ingest 请求失败: %v", err)
		}
		if _, _, err := client.postIngest(seedResult.AlertmanagerIntegrationID, seedResult.AlertmanagerToken,
			alertmanagerPayload(alertnameB, "critical", "firing", startsAt, map[string]string{"noise_scenario": scenarioTag})); err != nil {
			t.Fatalf("e2e: ingest 请求失败: %v", err)
		}
	}

	waitFor(t, 20*time.Second, time.Second, func() bool {
		post()

		a, okA, errA := findAlertByLabel(client, "alertname", alertnameA)
		b, okB, errB := findAlertByLabel(client, "alertname", alertnameB)
		if errA != nil || errB != nil || !okA || !okB {
			return false
		}
		if a.TeamID != teamID || b.TeamID != teamID {
			return false
		}
		if a.GroupKey == "" || a.GroupKey != b.GroupKey {
			return false
		}
		groupKey := a.GroupKey
		ids := []string{a.ID, b.ID}

		for _, s := range mockLark.Sent() {
			if s.Target == teamID && s.Msg.GroupKey == groupKey && sameIDSet(s.Msg.AlertIDs, ids) {
				return true
			}
		}
		return false
	}, "两条 alertname 不同的告警在 window 模式下归入同一固定组并发出一条聚合通知")
}
