package serviceobs_test

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"alert-system/internal/serviceobs"
	soctx "alert-system/internal/serviceobs/context"
)

func testContext() context.Context { return context.Background() }

// serviceobs.Subject 与 context.Subject 是同一个概念在两层的表示：
// ports 层不能 import context 包（依赖方向是 context -> ports），因此两处各有一份声明。
//
// 重复是有意的，但重复的两份必须**永不漂移**：Task 8 会把 context.Subject 映射进
// serviceobs.Subject，一侧多一个字段而另一侧没有，映射就会静默丢字段。这个测试让那种漂移
// 在编译测试阶段就失败，而不是等到某个权限判定莫名其妙失效。
func TestSubjectFieldSetsDoNotDrift(t *testing.T) {
	portFields := fieldSignature(reflect.TypeOf(serviceobs.Subject{}))
	assemblerFields := fieldSignature(reflect.TypeOf(soctx.Subject{}))

	if !reflect.DeepEqual(portFields, assemblerFields) {
		t.Fatalf("Subject 字段集漂移:\n  ports 层     = %v\n  assembler 层 = %v",
			portFields, assemblerFields)
	}
}

// fieldSignature 返回 "名字:类型" 的排序列表。
func fieldSignature(typ reflect.Type) []string {
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		out = append(out, field.Name+":"+field.Type.String())
	}
	sort.Strings(out)
	return out
}

// Purpose 的零值必须非法：装配层必须显式选择用途，不能靠默认值蒙对。
func TestPurposeZeroValueIsInvalid(t *testing.T) {
	var zero serviceobs.Purpose
	if zero.IsValid() {
		t.Fatal("Purpose 零值不应合法")
	}
	if zero.NeedsFocusEvidence() {
		t.Fatal("Purpose 零值不应允许证据查询")
	}
}

// 只有聚焦工作区允许扇出证据查询。
func TestOnlyFocusedWorkspaceNeedsEvidence(t *testing.T) {
	cases := map[serviceobs.Purpose]bool{
		serviceobs.PurposeDirectorySummary: false,
		serviceobs.PurposeOverview:         false,
		serviceobs.PurposeFocusedWorkspace: true,
	}
	for purpose, want := range cases {
		if !purpose.IsValid() {
			t.Fatalf("%q 应是合法用途", purpose)
		}
		if got := purpose.NeedsFocusEvidence(); got != want {
			t.Fatalf("%q NeedsFocusEvidence()=%t, want %t", purpose, got, want)
		}
	}
}

// 零值 Permissions 拒绝一切：默认从严。
func TestZeroPermissionsDenyEverything(t *testing.T) {
	var zero serviceobs.Permissions
	for _, permission := range []serviceobs.Permission{
		serviceobs.PermissionMetricsRead, serviceobs.PermissionLogsRead,
		serviceobs.PermissionTracesRead, serviceobs.PermissionFaultsRead,
	} {
		if zero.Allows(permission) {
			t.Fatalf("零值 Permissions 不应授予 %q", permission)
		}
	}
	if len(zero.Granted()) != 0 {
		t.Fatal("零值 Permissions 的 Granted 应为空")
	}

	granted := serviceobs.NewPermissions(serviceobs.PermissionMetricsRead, serviceobs.PermissionTracesRead)
	if !granted.Allows(serviceobs.PermissionMetricsRead) {
		t.Fatal("已授权的权限应被允许")
	}
	if granted.Allows(serviceobs.PermissionLogsRead) {
		t.Fatal("未授权的权限不应被允许")
	}
	// Granted 必须稳定排序，才能进 permission snapshot 指纹。
	list := granted.Granted()
	if len(list) != 2 || list[0] != serviceobs.PermissionMetricsRead || list[1] != serviceobs.PermissionTracesRead {
		t.Fatalf("Granted()=%v，应稳定排序", list)
	}
}

// 新 source key 必须与既有 telemetry key 保持兼容：SourceKeyPrometheus 就是它。
func TestSourceKeysAreStable(t *testing.T) {
	if serviceobs.SourceKeyPrometheus != serviceobs.SourceKeyTelemetry {
		t.Fatal("SourceKeyPrometheus 必须与 Phase 2 的 SourceKeyTelemetry 是同一个值")
	}
	expected := map[string]string{
		"prometheus": "prometheus_compatible",
		"logs":       "openobserve_logs",
		"traces":     "openobserve_traces",
		"access":     "openobserve_access",
	}
	actual := map[string]string{
		"prometheus": serviceobs.SourceKeyPrometheus,
		"logs":       serviceobs.SourceKeyOpenObserveLogs,
		"traces":     serviceobs.SourceKeyOpenObserveTraces,
		"access":     serviceobs.SourceKeyOpenObserveAccess,
	}
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("source key 漂移: %v", actual)
	}
}

// Phase 2 的 UnavailableObservationReader 行为必须逐字不变，且新增字段有确定的零语义。
func TestUnavailableReaderKeepsPhase2Semantics(t *testing.T) {
	reader := serviceobs.NewUnavailableObservationReader()
	facts, err := reader.ReadObservations(testContext(), serviceobs.ObservationRequest{})
	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if len(facts.Sources) != 1 || facts.Sources[0].ReasonCode != serviceobs.ReasonTelemetryNotConnected {
		t.Fatalf("source=%+v", facts.Sources)
	}
	if facts.Focus != serviceobs.FocusNotRequested {
		t.Fatalf("Focus=%q，未接入时应显式 not_requested", facts.Focus)
	}
	if facts.JourneyStages != nil || facts.TopologyHints != nil || facts.Issues != nil {
		t.Fatal("Phase 2 reader 不应产出 journey/hint/issue")
	}
}
