// Package events 是"告警变更实时推送"的进程内事件总线：PG 触发器在事务提交时
// pg_notify（见 migrations/000011），本包用一个专用连接 LISTEN 该 channel，
// 广播给进程内的多个 SSE 订阅者（internal/api/events.go）。前端收到的只是
// alert_id 信号，不带数据本身——收到后自己走 REST 重拉，权限与数据口径仍只有
// REST 一份，本包不做任何鉴权或数据查询。
package events

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// subscriberBufSize 是每个订阅者 channel 的缓冲大小：SSE 客户端网络抖动时能扛住
// 短暂积压，超过后按下面 Publish 的"丢弃"策略处理。
const subscriberBufSize = 16

// Bus 是进程内多订阅者广播器，可安全并发使用。
type Bus struct {
	mu   sync.Mutex
	subs map[int]chan string
	next int
}

// NewBus 构造一个空的 Bus。
func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan string)}
}

// Subscribe 注册一个新订阅者，返回只读 channel 与退订函数。调用方必须在不再消费
// 时调用退订函数，否则 Bus 会一直持有该 channel（内存泄漏）。
func (b *Bus) Subscribe() (<-chan string, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.next
	b.next++
	ch := make(chan string, subscriberBufSize)
	b.subs[id] = ch

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
	}
	return ch, unsubscribe
}

// Publish 把 alertID 广播给全部当前订阅者。必须非阻塞：某个订阅者的 channel
// 缓冲满了（消费太慢）就直接丢弃这一条，绝不能因为一个慢客户端反压 LISTEN 连接、
// 拖慢其他订阅者甚至让 RunListener 卡住。丢弃的只是"刷新信号"，前端本来就有轮询
// 兜底（或至少下一次变更会再触发一次通知），语义上可以接受，不追求必达。
func (b *Bus) Publish(alertID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- alertID:
		default:
		}
	}
}

// RunListener 是常驻 goroutine：用独立于 store 连接池的 pgx 连接执行
// LISTEN alert_changed，把收到的 payload（告警 UUID 文本）转发给 bus.Publish。
// 连接失败或断开时记日志并退避重连（1s 起，指数翻倍，上限 30s），ctx 取消时退出。
// 任何非 ctx-done 的错误都不能让本函数返回——这是系统运行期间的常驻组件。
func RunListener(ctx context.Context, dsn string, bus *Bus, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			logger.Error("events: 建立 LISTEN 连接失败，将重试", "err", err, "backoff", backoff)
			if !sleepOrDone(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		if _, err := conn.Exec(ctx, "LISTEN alert_changed"); err != nil {
			logger.Error("events: 执行 LISTEN 失败，将重试", "err", err, "backoff", backoff)
			_ = conn.Close(context.Background())
			if !sleepOrDone(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		logger.Info("events: LISTEN alert_changed 已建立")
		backoff = time.Second // 连接成功后重置退避

		for {
			notification, err := conn.WaitForNotification(ctx)
			if err != nil {
				if ctx.Err() != nil {
					_ = conn.Close(context.Background())
					return
				}
				logger.Error("events: LISTEN 连接断开，将重连", "err", err)
				break
			}
			bus.Publish(notification.Payload)
		}

		_ = conn.Close(context.Background())
		if !sleepOrDone(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// sleepOrDone 睡眠 d，若期间 ctx 被取消则提前返回 false（调用方应退出）。
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}
