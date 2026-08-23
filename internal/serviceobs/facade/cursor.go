package facade

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"alert-system/internal/serviceobs/view"
)

// CursorVersion 是游标编码版本。任何影响元组含义的改动都必须提升它，让旧游标显式
// 失败而不是被按新语义误解。
const CursorVersion = 1

// MaxCursorLength 是游标字符串的硬上限。它同时是解码前的第一道校验：超长输入直接
// 拒绝，不进 base64、不进 JSON、更不进 SQL。
const MaxCursorLength = 512

// maxCursorTextBytes 是游标里单个文本字段（名称、负责团队）的字节上界。
//
// 它存在的唯一原因是把游标长度钉死在 MaxCursorLength 之内：名称在数据库里是无界
// TEXT，一个超长名称会让游标越界，而"游标编码失败"等于翻页不可用。
//
// 代价被限制在可解释的最小范围：比较时**两侧**都按同一个前缀界收敛（见
// boundedKey），因此游标锚点自身永远被正确排除，不会重复也不会重启分页；只有那些
// 前 96 字节完全相同、且按全名与按 service ID 排序结果不同的行，才可能在两页之间
// 重复或跳过。这个边界写进 acceptance 已知限制。
const maxCursorTextBytes = 96

var (
	// ErrInvalidCursor 表示游标无法使用：超长、非法编码、版本不符，或绑定的查询指纹
	// 与当前请求不一致。它一律映射成 422，并且在任何数据库查询之前就返回。
	ErrInvalidCursor = errors.New("serviceobs/facade: 游标非法")
	// ErrCursorTooLong 表示要编码的游标超过硬上限。它是实现缺陷（文本界没生效），
	// 不是用户输入问题。
	ErrCursorTooLong = errors.New("serviceobs/facade: 游标超过长度上限")
)

// cursorPayload 是游标的载荷。
//
// 字段名刻意压到一个字符：游标必须落在 512 字符内，而它要携带完整排序元组 + service
// ID + 版本 + 查询指纹。additionalProperties 语义由 DisallowUnknownFields 提供，
// 未知字段一律拒绝，不猜测兼容。
type cursorPayload struct {
	Version     int    `json:"v"`
	Fingerprint string `json:"f"`
	HealthRank  int    `json:"h"`
	Confirmed   int    `json:"i"`
	Faults      int    `json:"a"`
	Tier        string `json:"t"`
	Name        string `json:"n"`
	Owner       string `json:"o"`
	DataRank    int    `json:"d"`
	ObservedAt  int64  `json:"b"`
	ServiceID   string `json:"s"`
}

// encodeCursor 把一行的排序元组编成 opaque base64url 游标。
func encodeCursor(key view.DirectoryKey, fingerprint string) (string, error) {
	bounded := boundedKey(key)
	raw, err := json.Marshal(cursorPayload{
		Version:     CursorVersion,
		Fingerprint: fingerprint,
		HealthRank:  bounded.HealthRank,
		Confirmed:   bounded.ConfirmedImpacts,
		Faults:      bounded.ActiveFaults,
		Tier:        bounded.Tier,
		Name:        bounded.Name,
		Owner:       bounded.OwnerTeamName,
		DataRank:    bounded.DataRank,
		ObservedAt:  bounded.ObservedAtUnixMilli,
		ServiceID:   bounded.ServiceID,
	})
	if err != nil {
		return "", fmt.Errorf("serviceobs/facade: 编码游标失败: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > MaxCursorLength {
		return "", fmt.Errorf("%w: %d > %d", ErrCursorTooLong, len(encoded), MaxCursorLength)
	}
	return encoded, nil
}

// decodeCursor 解码并校验游标。
//
// 校验顺序就是安全顺序：长度 -> base64 -> JSON（拒绝未知字段）-> 版本 -> 查询指纹。
// 任何一步失败都返回 ErrInvalidCursor，调用方在发起任何查询之前就 422。
func decodeCursor(encoded, fingerprint string) (view.DirectoryKey, error) {
	if encoded == "" {
		return view.DirectoryKey{}, fmt.Errorf("%w: 游标为空", ErrInvalidCursor)
	}
	if len(encoded) > MaxCursorLength {
		return view.DirectoryKey{}, fmt.Errorf("%w: 长度 %d 超过上限 %d",
			ErrInvalidCursor, len(encoded), MaxCursorLength)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return view.DirectoryKey{}, fmt.Errorf("%w: 不是合法 base64url", ErrInvalidCursor)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var payload cursorPayload
	if err := decoder.Decode(&payload); err != nil {
		return view.DirectoryKey{}, fmt.Errorf("%w: 载荷无法解析", ErrInvalidCursor)
	}
	if payload.Version != CursorVersion {
		return view.DirectoryKey{}, fmt.Errorf("%w: 版本 %d 不是当前版本 %d",
			ErrInvalidCursor, payload.Version, CursorVersion)
	}
	if payload.Fingerprint != fingerprint {
		// 换了筛选/排序/方向就必须重新从第一页开始：把旧位置套到新查询上，
		// 会静默跳过或重复大段结果。
		return view.DirectoryKey{}, fmt.Errorf("%w: 游标绑定的查询与当前请求不一致", ErrInvalidCursor)
	}
	if payload.ServiceID == "" {
		return view.DirectoryKey{}, fmt.Errorf("%w: 缺少 service ID 锚点", ErrInvalidCursor)
	}
	return view.DirectoryKey{
		HealthRank:          payload.HealthRank,
		ConfirmedImpacts:    payload.Confirmed,
		ActiveFaults:        payload.Faults,
		Tier:                payload.Tier,
		Name:                payload.Name,
		OwnerTeamName:       payload.Owner,
		DataRank:            payload.DataRank,
		ObservedAtUnixMilli: payload.ObservedAt,
		ServiceID:           payload.ServiceID,
	}, nil
}

// afterCursor 判断一行是否落在游标之后。
//
// 它只比较排序元组 + service ID，**不要求**在新算出的列表里找到游标那一行：游标行
// 在两页之间被删掉时，比较仍然给出同一个插入位置，翻页因此继续而不是失败或从头开始。
// 两侧都经过 boundedKey，所以游标锚点自身必然被排除（元组与 service ID 都相等 ->
// 不小于 -> 不返回）。
func afterCursor(cursor, candidate view.DirectoryKey, spec view.DirectorySort) bool {
	return view.DirectoryKeyLess(cursor, boundedKey(candidate), spec)
}

// boundedKey 把文本字段收敛到游标的前缀界。
//
// 关键点是**两侧同时收敛**：只收敛游标一侧会让"游标行自己"因为全名更长而被判成
// 大于游标，于是每一页都把上一页最后一行再返回一次。
func boundedKey(key view.DirectoryKey) view.DirectoryKey {
	key.Name = boundText(key.Name)
	key.OwnerTeamName = boundText(key.OwnerTeamName)
	return key
}

// boundText 按字节上界截断，并保持 UTF-8 边界：截断出半个 rune 会让游标里出现无效
// 编码，进而让 JSON 编解码结果与原值不可比较。
func boundText(value string) string {
	if len(value) <= maxCursorTextBytes {
		return value
	}
	bounded := value[:maxCursorTextBytes]
	for len(bounded) > 0 && !utf8.ValidString(bounded) {
		bounded = bounded[:len(bounded)-1]
	}
	return bounded
}

// queryFingerprint 是游标与查询之间的绑定。
//
// 它覆盖**全部** filter、sort 与 direction：任何一项变化都必须让旧游标失效。limit
// 刻意不参与——换页大小是合法操作，不改变排序序列本身。
func queryFingerprint(query DirectoryQuery) string {
	parts := []string{
		"v" + strconv.Itoa(CursorVersion),
		"environment=" + query.Environment,
		"team_id=" + query.TeamID,
		"tier=" + query.Tier,
		"q=" + query.Query,
		"health=" + strings.Join(normalizedHealthFilter(query.Health), ","),
		"data_status=" + strings.Join(normalizedDataFilter(query.DataStatus), ","),
		"sort=" + string(query.Sort.Key),
		"direction=" + directionToken(query.Sort.Descending),
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	// 16 字节足够抗碰撞：游标不是凭据，只是"这是同一个查询"的绑定。
	return base64.RawURLEncoding.EncodeToString(digest[:16])
}

func directionToken(descending bool) string {
	if descending {
		return "desc"
	}
	return "asc"
}
