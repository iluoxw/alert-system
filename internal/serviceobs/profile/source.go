package profile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// MaxDocumentBytes 是单个 Profile 文档的硬上限。Profile 是声明式配置，1 MiB 已经
	// 远超任何真实 Profile；更大的文件只可能是误放的数据文件或攻击载荷。
	MaxDocumentBytes = 1 << 20
	// MaxCandidateFiles 是目录里允许的候选文档数量上限。超限说明目录被误配（例如指向了
	// 一个日志目录），是部署错误而不是单个 Profile 的问题，因此让 Registry 构造失败。
	MaxCandidateFiles = 1000
)

// Source 读取失败时的 typed 原因。Registry 用 errors.Is 把它们映射成稳定 reason code，
// 不靠匹配错误文案。
var (
	ErrDocumentTooLarge   = errors.New("profile: 文档超过大小上限")
	ErrNotRegularJSONFile = errors.New("profile: 不是目录直属的普通 .json 文件")
)

// Source 提供候选 Profile 文档。它是注入点：生产用 DirSource，测试用 MapSource，
// profile 包本身不读环境变量、不碰数据库。
type Source interface {
	// List 返回稳定排序的候选文档名。目录本身不可读必须返回 error（让启动失败），
	// 而不是返回空列表把"读不到"伪装成"没配置"。
	List() ([]string, error)
	// Read 返回单个文档的原始字节。
	Read(name string) ([]byte, error)
}

// DirSource 从一个本地目录读取 Profile。它只接受目录直属的普通 .json 文件：
// symlink 会被拒绝（避免通过链接读到目录外的任意文件），子目录不递归，名字里带路径
// 分隔符或 .. 的一律拒绝。
type DirSource struct {
	dir string
}

// NewDirSource 构造一个目录 Source。dir 为空字符串时应由调用方直接跳过（空 registry），
// 这里不把空路径当成合法目录。
func NewDirSource(dir string) *DirSource { return &DirSource{dir: dir} }

// Dir 返回配置的目录路径，仅供装配自检使用；它不能进日志和 API 响应。
func (s *DirSource) Dir() string { return s.dir }

func (s *DirSource) List() ([]string, error) {
	if strings.TrimSpace(s.dir) == "" {
		return nil, fmt.Errorf("profile: Profile 目录路径为空")
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("profile: 读取 Profile 目录失败: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		// entry.Type() 来自 lstat，因此 symlink 在这里就会被识别为非普通文件。
		if !entry.Type().IsRegular() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) > MaxCandidateFiles {
		return nil, fmt.Errorf("profile: Profile 目录候选文件数=%d 超过上限 %d，目录配置可疑",
			len(names), MaxCandidateFiles)
	}
	sort.Strings(names)
	return names, nil
}

// Read 读取单个 Profile 文档。
//
// 所有检查都绑定到同一个已打开的文件描述符，而不是反复用路径去 stat/读取：路径在两次
// 系统调用之间可以被换成 symlink 或另一个文件（TOCTOU），"先按路径检查、再按路径读取"
// 会读到检查过的那个文件之外的内容。
//
// 顺序上先 Lstat 再 Open：Open 会跟随 symlink，所以拒绝直接 symlink 必须发生在打开之前；
// 打开之后再用 fd 的 Stat 加 os.SameFile 确认拿到的仍是刚才检查过的那个普通文件。
// 读取本身经 io.LimitReader 有界，因此大小上限在内存分配之前生效——被换成超大文件也不会
// 先整份读进内存再报错。
func (s *DirSource) Read(name string) ([]byte, error) {
	if err := validateDocumentName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, name)

	lstatInfo, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("profile: 无法 stat %s: %w", name, err)
	}
	if !lstatInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", ErrNotRegularJSONFile, name)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("profile: 打开 %s 失败: %w", name, err)
	}
	defer file.Close()

	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("profile: 无法 stat 已打开的 %s: %w", name, err)
	}
	// 身份检查：Lstat 与 Open 之间文件被替换（rename、换成 symlink 指向别处）会让
	// dev+inode 不同，这里直接拒绝，而不是读一份来源不明的内容。
	if !openedInfo.Mode().IsRegular() || !os.SameFile(lstatInfo, openedInfo) {
		return nil, fmt.Errorf("%w: %s 在检查与打开之间被替换", ErrNotRegularJSONFile, name)
	}
	if openedInfo.Size() > MaxDocumentBytes {
		return nil, fmt.Errorf("%w: %s 为 %d 字节，上限 %d",
			ErrDocumentTooLarge, name, openedInfo.Size(), MaxDocumentBytes)
	}

	// 多读 1 字节，用来区分"正好等于上限"（合法）与"超过上限"（拒绝）。即使 fd 上报的
	// size 不可信，这条有界读取仍然是硬上限。
	raw, err := io.ReadAll(io.LimitReader(file, MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("profile: 读取 %s 失败: %w", name, err)
	}
	if len(raw) > MaxDocumentBytes {
		return nil, fmt.Errorf("%w: %s 超过上限 %d", ErrDocumentTooLarge, name, MaxDocumentBytes)
	}
	return raw, nil
}

// validateDocumentName 拒绝目录穿越与非 .json 名字。Read 的入参理论上都来自 List，
// 但这是安全边界，不能依赖调用方守规矩。
func validateDocumentName(name string) error {
	if name == "" || name != filepath.Base(name) ||
		strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("%w: 非法文档名 %q", ErrNotRegularJSONFile, name)
	}
	if !strings.HasSuffix(name, ".json") {
		return fmt.Errorf("%w: %q 不是 .json 文档", ErrNotRegularJSONFile, name)
	}
	return nil
}

// MapSource 是内存 Source，供测试与 fixture 模式使用。
type MapSource struct {
	Documents map[string][]byte
	// ListErr 非 nil 时模拟"目录本身不可读"。
	ListErr error
}

func (s *MapSource) List() ([]string, error) {
	if s.ListErr != nil {
		return nil, s.ListErr
	}
	names := make([]string, 0, len(s.Documents))
	for name := range s.Documents {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		names = append(names, name)
	}
	if len(names) > MaxCandidateFiles {
		return nil, fmt.Errorf("profile: 候选文件数=%d 超过上限 %d", len(names), MaxCandidateFiles)
	}
	sort.Strings(names)
	return names, nil
}

func (s *MapSource) Read(name string) ([]byte, error) {
	if err := validateDocumentName(name); err != nil {
		return nil, err
	}
	raw, ok := s.Documents[name]
	if !ok {
		return nil, fmt.Errorf("profile: 文档 %s 不存在", name)
	}
	if len(raw) > MaxDocumentBytes {
		return nil, fmt.Errorf("%w: %s 为 %d 字节，上限 %d", ErrDocumentTooLarge, name, len(raw), MaxDocumentBytes)
	}
	return raw, nil
}
