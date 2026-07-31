import { useQuery } from '@tanstack/react-query'
import { listTeamsLite, listUsersLite } from '../api/client'

// staleTime 5 分钟：团队/用户名单变化很慢（远慢于告警列表 10s 轮询节奏），
// 没必要跟着告警一起频繁重新拉取。
const nameMapStaleTime = 5 * 60 * 1000

// teamsLiteQueryKey/usersLiteQueryKey 导出给需要"完整列表"的调用方（如
// AlertList.tsx 的团队筛选 Select）复用同一 queryKey，与本 hook 共享缓存，
// 避免两处各发一次请求。
export const teamsLiteQueryKey = ['directory', 'teams'] as const
export const usersLiteQueryKey = ['directory', 'users'] as const

// fallbackName 在映射查不到时退回 UUID 前 8 位——兼顾"仍能定位是哪条记录"与
// "不把 36 位裸 UUID 糊满整个单元格"。
function fallbackName(id: string): string {
  return id.slice(0, 8)
}

// useNameMaps 把 team_id/acked_by 等 UUID 解析成人类可读名字（R4 member 日常
// 可用），依赖 internal/api/directory.go 的两个轻量端点（登录即可，非 admin）。
// 查询失败时 data 保持 undefined，map 为空，teamName/userName 优雅退回
// fallbackName，不阻塞告警列表/详情的渲染——名字展示是锦上添花，不是主流程。
export function useNameMaps() {
  const teamsQuery = useQuery({
    queryKey: teamsLiteQueryKey,
    queryFn: async () => (await listTeamsLite()).teams,
    staleTime: nameMapStaleTime,
  })
  const usersQuery = useQuery({
    queryKey: usersLiteQueryKey,
    queryFn: async () => (await listUsersLite()).users,
    staleTime: nameMapStaleTime,
  })

  const teamMap = new Map((teamsQuery.data ?? []).map((t) => [t.id, t.name]))
  const userMap = new Map((usersQuery.data ?? []).map((u) => [u.id, u.display_name]))

  const teamName = (id?: string | null): string => {
    if (!id) return '-'
    return teamMap.get(id) ?? fallbackName(id)
  }

  const userName = (id?: string | null): string => {
    if (!id) return '-'
    return userMap.get(id) ?? fallbackName(id)
  }

  return { teamName, userName }
}
