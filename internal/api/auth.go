package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"alert-system/internal/store"
)

// ErrInvalidCredentials 是登录失败的唯一错误：用户不存在与密码错误必须返回完全
// 相同的错误（防用户名枚举，见下方 dummyPasswordHash 的用法说明）。调用方（HTTP
// 层）据此渲染同一条响应文案与状态码，不区分两种失败原因。
var ErrInvalidCredentials = errors.New("api: 用户名或密码错误")

// dummyPasswordHash 是进程启动时算好的固定 bcrypt 哈希（对应一个从未使用、随进程
// 重启而变化的随机密码），仅用于"用户不存在"分支也去跑一次
// bcrypt.CompareHashAndPassword——bcrypt 比较的耗时与哈希本身的 cost 成正比、与被
// 比较的密码内容无关，所以不管是哪个用户名，每次登录尝试的总耗时都落在同一量级，
// 观测方无法通过响应时间区分"用户名不存在"与"用户名存在但密码错误"（时序侧信道防
// 用户名枚举）。用 var 而非硬编码字符串常量，避免手写 bcrypt 哈希格式出错。
var dummyPasswordHash = mustGenerateDummyHash()

func mustGenerateDummyHash() string {
	hash, err := bcrypt.GenerateFromPassword([]byte("dummy-password-never-used"), bcrypt.DefaultCost)
	if err != nil {
		panic(fmt.Sprintf("api: 生成防枚举 dummy hash 失败: %v", err))
	}
	return string(hash)
}

// UserStore 抽象 store.UserRepo 用到的方法，供本包在单测里注入内存 fake。
// *store.UserRepo 天然满足这个接口。GetByUsername 供登录使用，GetByID 供会话
// 中间件在每次请求时重新加载用户（校验 enabled 状态是否被中途撤销，见 middleware.go）。
type UserStore interface {
	GetByUsername(ctx context.Context, tx pgx.Tx, username string) (*store.User, error)
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.User, error)
}

// Authenticator 是登录鉴权的抽象（R8.1）：本期只有密码实现，OIDC 作为未来的第二个
// 实现接入同一接口，HTTP 层（server.go 的 /auth/login）不感知具体是哪种实现。
type Authenticator interface {
	// Authenticate 校验用户名/密码，成功返回该用户；失败统一返回 ErrInvalidCredentials
	// （不区分用户不存在/密码错误/账号禁用，避免向调用方泄露除"能否登录"外的任何信息）。
	Authenticate(ctx context.Context, tx pgx.Tx, username, password string) (*store.User, error)
}

// PasswordAuthenticator 是 Authenticator 的密码实现：bcrypt 校验 users.password_hash。
type PasswordAuthenticator struct {
	users UserStore
}

// NewPasswordAuthenticator 构造 PasswordAuthenticator。users 通常直接传 *store.UserRepo。
func NewPasswordAuthenticator(users UserStore) *PasswordAuthenticator {
	return &PasswordAuthenticator{users: users}
}

// Authenticate 实现 Authenticator。
func (a *PasswordAuthenticator) Authenticate(ctx context.Context, tx pgx.Tx, username, password string) (*store.User, error) {
	u, err := a.users.GetByUsername(ctx, tx, username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("api: 查询用户失败: %w", err)
	}

	hash := dummyPasswordHash
	if u != nil {
		hash = u.PasswordHash
	}
	// 无论用户是否存在都执行这次比较：分支耗时对齐，见 dummyPasswordHash 的文档。
	compareErr := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))

	if u == nil || !u.Enabled || compareErr != nil {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}
