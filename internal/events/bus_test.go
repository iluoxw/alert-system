package events

import (
	"testing"
	"time"
)

func TestBus_Publish_BroadcastsToAllSubscribers(t *testing.T) {
	bus := NewBus()
	ch1, unsub1 := bus.Subscribe()
	defer unsub1()
	ch2, unsub2 := bus.Subscribe()
	defer unsub2()

	bus.Publish("alert-1")

	for i, ch := range []<-chan string{ch1, ch2} {
		select {
		case got := <-ch:
			if got != "alert-1" {
				t.Fatalf("订阅者 %d 收到 %q，期望 alert-1", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("订阅者 %d 超时未收到消息", i)
		}
	}
}

func TestBus_Unsubscribe_StopsReceiving(t *testing.T) {
	bus := NewBus()
	ch, unsub := bus.Subscribe()
	unsub()

	bus.Publish("alert-1")

	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("退订后仍收到消息: %q", got)
		}
		// channel 已关闭，符合预期。
	case <-time.After(100 * time.Millisecond):
		t.Fatal("退订后 channel 既未关闭也未收到消息")
	}
}

// TestBus_Publish_SlowSubscriberDoesNotBlock 验证慢订阅者（缓冲已满、不消费）不会
// 反压 Publish：这是 Bus 的核心取舍——丢弃信号也不能阻塞发布方（listener goroutine）。
func TestBus_Publish_SlowSubscriberDoesNotBlock(t *testing.T) {
	bus := NewBus()
	slow, unsubSlow := bus.Subscribe()
	defer unsubSlow()
	fast, unsubFast := bus.Subscribe()
	defer unsubFast()

	// 灌满慢订阅者的缓冲区（不消费），再多发几条：Publish 不应阻塞。
	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberBufSize+5; i++ {
			bus.Publish("alert-x")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish 在慢订阅者缓冲满后被阻塞")
	}

	// 快订阅者理应能收到最后一条（不受慢订阅者影响，只受自身缓冲影响）。
	select {
	case <-fast:
	case <-time.After(time.Second):
		t.Fatal("快订阅者未收到任何消息")
	}

	// 确认慢订阅者的 channel 确实被填满（缓冲区大小上限），验证丢弃发生在缓冲区而非
	// 全部消息都被送达。
	drained := 0
	for {
		select {
		case <-slow:
			drained++
		default:
			if drained != subscriberBufSize {
				t.Fatalf("慢订阅者收到 %d 条，期望缓冲区大小 %d（多余的应被丢弃）", drained, subscriberBufSize)
			}
			return
		}
	}
}
