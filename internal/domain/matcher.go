package domain

import (
	"fmt"
	"regexp"
	"sync"
)

// MatchOp 是 matcher 支持的比较操作符，语义对齐 Alertmanager silence matcher：
// = 等于，!= 不等于，=~ 正则匹配，!~ 正则不匹配。
type MatchOp string

const (
	MatchEqual     MatchOp = "="
	MatchNotEqual  MatchOp = "!="
	MatchRegexp    MatchOp = "=~"
	MatchNotRegexp MatchOp = "!~"
)

// Matcher 是单条标签匹配规则：将 labels[Name] 的值按 Op 与 Value 比较。
// 缺失的标签按空字符串处理（与 Alertmanager 一致），使 `=~ ".*"` 可匹配缺失标签、
// `!= "x"` 对缺失标签为真。
type Matcher struct {
	Name  string  `json:"name"`
	Op    MatchOp `json:"op"`
	Value string  `json:"value"`
}

// Validate 校验 Op 合法、且正则类操作符的 Value 能编译为合法正则。
func (m Matcher) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("matcher: name 不能为空")
	}
	switch m.Op {
	case MatchEqual, MatchNotEqual:
		return nil
	case MatchRegexp, MatchNotRegexp:
		if _, err := compileAnchored(m.Value); err != nil {
			return fmt.Errorf("matcher: 非法正则 %q: %w", m.Value, err)
		}
		return nil
	default:
		return fmt.Errorf("matcher: 非法操作符 %q", m.Op)
	}
}

// matches 判断单个标签值是否命中该 matcher。正则编译失败时视为不命中
// （=~ 与 !~ 均返回 false），避免非法配置意外匹配全量告警；合法配置应先经 Validate 拦截。
func (m Matcher) matches(actual string) bool {
	switch m.Op {
	case MatchEqual:
		return actual == m.Value
	case MatchNotEqual:
		return actual != m.Value
	case MatchRegexp:
		re, err := compileAnchored(m.Value)
		if err != nil {
			return false
		}
		return re.MatchString(actual)
	case MatchNotRegexp:
		re, err := compileAnchored(m.Value)
		if err != nil {
			return false
		}
		return !re.MatchString(actual)
	default:
		return false
	}
}

// MatcherSet 是一组 matcher，Matches 为 AND 语义：全部命中才算匹配。
// 供静默（R10）、路由（R5.1）、策略绑定（R13.5）三处复用。
type MatcherSet []Matcher

// Matches 对给定 labels 求值。空 MatcherSet 始终返回 false，
// 防止空配置（如未填写 matchers 的路由/静默规则）误匹配全量告警。
func (ms MatcherSet) Matches(labels map[string]string) bool {
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !m.matches(labels[m.Name]) {
			return false
		}
	}
	return true
}

// Validate 校验集合内每条 matcher。
func (ms MatcherSet) Validate() error {
	for i, m := range ms {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("matcher[%d]: %w", i, err)
		}
	}
	return nil
}

// regexCache 缓存按 Alertmanager 语义整串锚定（^(?:pattern)$）后编译的正则，
// 避免同一 pattern 在高频 Matches 调用中重复编译；sync.Map 保证并发安全。
var regexCache sync.Map // map[string]*regexp.Regexp

func compileAnchored(pattern string) (*regexp.Regexp, error) {
	if cached, ok := regexCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return nil, err
	}
	actual, _ := regexCache.LoadOrStore(pattern, re)
	return actual.(*regexp.Regexp), nil
}
