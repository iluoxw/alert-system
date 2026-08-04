import { useEffect, useRef } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { apiV1Url } from '../config/runtime'

// 节流窗口：告警风暴时后端可能密集推送 alert_changed 事件，2s 内的多条事件
// 合并为一次 invalidate，避免每条事件都触发一次重拉。invalidateQueries 只
// 标记 stale，真正发起网络请求的只有当前挂载的 query，代价可控，不需要按
// 具体 alert_id 精确定位。
const invalidateThrottleMs = 2_000

// useAlertEvents 订阅后端 SSE 推送（GET /api/v1/events，session cookie 随
// EventSource 同源请求自动带上），收到 alert_changed 事件后节流 invalidate
// 告警列表/详情/噪声画像三类 query。只应在 AppLayout（登录后公共外壳）里
// 调用一次，保证全站共享同一条连接；随宿主组件卸载而关闭，不产生额外重试。
//
// EventSource 断线由浏览器原生自动重连，这里不写重连逻辑。推送是轮询之外的
// 增强而非依赖——onerror 静默降级，不打印告警内容也不弹全局提示，坏了还有
// AlertList/AlertDetail 的 15s 轮询兜底。
export function useAlertEvents(): void {
  const queryClient = useQueryClient()
  const pendingRef = useRef(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    const es = new EventSource(apiV1Url('/events'), { withCredentials: true })

    const scheduleInvalidate = () => {
      if (pendingRef.current) return
      pendingRef.current = true
      timerRef.current = setTimeout(() => {
        pendingRef.current = false
        queryClient.invalidateQueries({ queryKey: ['alerts'] })
        queryClient.invalidateQueries({ queryKey: ['alert-detail'] })
        queryClient.invalidateQueries({ queryKey: ['alert-stats'] })
      }, invalidateThrottleMs)
    }

    es.addEventListener('alert_changed', scheduleInvalidate)
    es.onerror = () => {
      // 静默降级：连接失败/服务重启期间不打扰用户，浏览器会自动重连。
    }

    return () => {
      if (timerRef.current !== null) clearTimeout(timerRef.current)
      es.close()
    }
  }, [queryClient])
}
