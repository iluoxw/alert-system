package domain

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestMatcher_Matches(t *testing.T) {
	cases := []struct {
		name    string
		matcher Matcher
		labels  map[string]string
		want    bool
	}{
		// = 等于
		{"eq 命中", Matcher{Name: "severity", Op: MatchEqual, Value: "critical"}, map[string]string{"severity": "critical"}, true},
		{"eq 不命中", Matcher{Name: "severity", Op: MatchEqual, Value: "critical"}, map[string]string{"severity": "warning"}, false},
		{"eq 缺失标签视为空串", Matcher{Name: "team", Op: MatchEqual, Value: ""}, map[string]string{}, true},
		{"eq 缺失标签不等于非空值", Matcher{Name: "team", Op: MatchEqual, Value: "sre"}, map[string]string{}, false},

		// != 不等于
		{"ne 命中", Matcher{Name: "severity", Op: MatchNotEqual, Value: "critical"}, map[string]string{"severity": "warning"}, true},
		{"ne 不命中", Matcher{Name: "severity", Op: MatchNotEqual, Value: "critical"}, map[string]string{"severity": "critical"}, false},
		{"ne 对缺失标签为真", Matcher{Name: "team", Op: MatchNotEqual, Value: "sre"}, map[string]string{}, true},

		// =~ 正则匹配
		{"regexp 命中", Matcher{Name: "host", Op: MatchRegexp, Value: "web-.*"}, map[string]string{"host": "web-01"}, true},
		{"regexp 不命中", Matcher{Name: "host", Op: MatchRegexp, Value: "web-.*"}, map[string]string{"host": "db-01"}, false},
		{"regexp 必须整串锚定：不匹配子串", Matcher{Name: "host", Op: MatchRegexp, Value: "web"}, map[string]string{"host": "web-01"}, false},
		{"regexp 全匹配任意串可命中缺失标签", Matcher{Name: "team", Op: MatchRegexp, Value: ".*"}, map[string]string{}, true},
		{"regexp 非法正则视为不命中", Matcher{Name: "host", Op: MatchRegexp, Value: "("}, map[string]string{"host": "web-01"}, false},

		// !~ 正则不匹配
		{"not-regexp 命中", Matcher{Name: "host", Op: MatchNotRegexp, Value: "web-.*"}, map[string]string{"host": "db-01"}, true},
		{"not-regexp 不命中", Matcher{Name: "host", Op: MatchNotRegexp, Value: "web-.*"}, map[string]string{"host": "web-01"}, false},
		{"not-regexp 对缺失标签视语义而定", Matcher{Name: "team", Op: MatchNotRegexp, Value: "sre"}, map[string]string{}, true},
		{"not-regexp 非法正则视为不命中", Matcher{Name: "host", Op: MatchNotRegexp, Value: "("}, map[string]string{"host": "web-01"}, false},

		// 非法 Op：matches 内部兜底为不命中（Validate 才是拦截点）
		{"非法 op 视为不命中", Matcher{Name: "host", Op: "??", Value: "x"}, map[string]string{"host": "x"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatcherSet{tc.matcher}.Matches(tc.labels)
			if got != tc.want {
				t.Errorf("Matches(%+v, %v) = %v, want %v", tc.matcher, tc.labels, got, tc.want)
			}
		})
	}
}

func TestMatcherSet_Matches_AND语义(t *testing.T) {
	ms := MatcherSet{
		{Name: "service.name", Op: MatchEqual, Value: "api"},
		{Name: "env", Op: MatchRegexp, Value: "prod|staging"},
		{Name: "team", Op: MatchNotEqual, Value: "unassigned"},
	}

	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"全部命中", map[string]string{"service.name": "api", "env": "prod", "team": "sre"}, true},
		{"其中一条不命中即整体不命中", map[string]string{"service.name": "api", "env": "dev", "team": "sre"}, false},
		{"team 缺失时 != unassigned 为真但其他条件仍需满足", map[string]string{"service.name": "api", "env": "staging"}, true},
		{"关键字段缺失导致不命中", map[string]string{"env": "prod"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ms.Matches(tc.labels); got != tc.want {
				t.Errorf("Matches(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

func TestMatcherSet_Matches_空集合返回false(t *testing.T) {
	var ms MatcherSet
	if ms.Matches(map[string]string{"any": "value"}) {
		t.Fatal("空 MatcherSet 不应匹配任何标签集")
	}
	if (MatcherSet{}).Matches(nil) {
		t.Fatal("空 MatcherSet 面对 nil labels 也不应匹配")
	}
}

func TestMatcher_Validate(t *testing.T) {
	cases := []struct {
		name    string
		matcher Matcher
		wantErr bool
	}{
		{"合法 eq", Matcher{Name: "a", Op: MatchEqual, Value: "b"}, false},
		{"合法 ne", Matcher{Name: "a", Op: MatchNotEqual, Value: "b"}, false},
		{"合法正则", Matcher{Name: "a", Op: MatchRegexp, Value: "b.*"}, false},
		{"合法反正则", Matcher{Name: "a", Op: MatchNotRegexp, Value: "b.*"}, false},
		{"非法正则报错", Matcher{Name: "a", Op: MatchRegexp, Value: "("}, true},
		{"非法反正则报错", Matcher{Name: "a", Op: MatchNotRegexp, Value: "("}, true},
		{"非法操作符报错", Matcher{Name: "a", Op: "~~", Value: "b"}, true},
		{"空 name 报错", Matcher{Name: "", Op: MatchEqual, Value: "b"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.matcher.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestMatcherSet_Validate_聚合首个错误(t *testing.T) {
	ms := MatcherSet{
		{Name: "a", Op: MatchEqual, Value: "1"},
		{Name: "b", Op: "bad", Value: "2"},
	}
	if err := ms.Validate(); err == nil {
		t.Fatal("期望第二条 matcher 的非法 Op 被 Validate 捕获")
	}
}

func TestMatcher_JSON反序列化(t *testing.T) {
	raw := `[
		{"name":"severity","op":"=","value":"critical"},
		{"name":"host","op":"=~","value":"web-.*"}
	]`
	var ms MatcherSet
	if err := json.Unmarshal([]byte(raw), &ms); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("期望 2 条 matcher，got %d", len(ms))
	}
	if ms[0].Name != "severity" || ms[0].Op != MatchEqual || ms[0].Value != "critical" {
		t.Errorf("第一条 matcher 反序列化错误: %+v", ms[0])
	}
	if ms[1].Op != MatchRegexp {
		t.Errorf("第二条 matcher Op 应为 =~，got %q", ms[1].Op)
	}
	if !ms.Matches(map[string]string{"severity": "critical", "host": "web-01"}) {
		t.Error("反序列化后的 MatcherSet 应能正常求值")
	}
}

func TestMatcher_JSON反序列化_非法Op不报错但Validate报错(t *testing.T) {
	raw := `{"name":"a","op":"bogus","value":"x"}`
	var m Matcher
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("JSON 反序列化本身不应因非法 Op 报错: %v", err)
	}
	if err := m.Validate(); err == nil {
		t.Fatal("Validate 应拦截非法 Op")
	}
}

// TestMatcher_Matches_并发安全 用 -race 验证 regexCache 在并发求值下不产生数据竞争。
func TestMatcher_Matches_并发安全(t *testing.T) {
	ms := MatcherSet{
		{Name: "host", Op: MatchRegexp, Value: "web-[0-9]+"},
		{Name: "env", Op: MatchNotRegexp, Value: "dev"},
	}
	labelSets := []map[string]string{
		{"host": "web-01", "env": "prod"},
		{"host": "db-01", "env": "prod"},
		{"host": "web-02", "env": "dev"},
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ls := labelSets[i%len(labelSets)]
			ms.Matches(ls)
		}(i)
	}
	wg.Wait()
}
