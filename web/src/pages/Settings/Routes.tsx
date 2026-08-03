import { useQuery } from '@tanstack/react-query'
import { Form, InputNumber, Select } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { CrudTable } from '../../components/CrudTable'
import { MatchersEditor } from '../../components/MatchersEditor'
import type { MatcherSet, RouteConfig, RouteRequest } from '../../api/client'
import { createRoute, deleteRoute, listRoutes, listTeams, updateRoute } from '../../api/client'

// 命名 RoutesPage 而不是 Routes，避免与 react-router-dom 的 <Routes> 组件重名
// （App.tsx 同时需要两者）。
export function RoutesPage() {
  // target_team_id 用下拉选择而不是让管理员手填 UUID，复用 teams 的查询缓存
  // （与 Settings/Teams.tsx 共用 queryKey，避免重复请求）。
  const { data: teamsData } = useQuery({ queryKey: ['admin', 'teams'], queryFn: async () => (await listTeams()).teams })
  const teamOptions = (teamsData ?? []).map((t) => ({ value: t.id, label: t.name }))

  const columns: ColumnsType<RouteConfig> = [
    { title: '优先级', dataIndex: 'priority', key: 'priority', width: 90 },
    {
      title: '匹配条件',
      dataIndex: 'matchers',
      key: 'matchers',
      render: (matchers: MatcherSet) =>
        matchers.length === 0 ? '（空，匹配全局默认）' : matchers.map((m) => `${m.name}${m.op}${m.value}`).join(', '),
    },
    {
      title: '目标团队',
      dataIndex: 'target_team_id',
      key: 'target_team_id',
      render: (id: string) => teamOptions.find((o) => o.value === id)?.label ?? id,
    },
    {
      title: '分组模式',
      dataIndex: 'group_mode',
      key: 'group_mode',
      width: 100,
      render: (mode: string) => (mode === 'window' ? '窗口全归并' : '按标签分组'),
    },
    { title: '分组等待(s)', dataIndex: 'group_wait_seconds', key: 'group_wait_seconds', width: 110 },
    { title: '分组间隔(s)', dataIndex: 'group_interval_seconds', key: 'group_interval_seconds', width: 110 },
    { title: '重复间隔(s)', dataIndex: 'repeat_interval_seconds', key: 'repeat_interval_seconds', width: 110 },
    { title: '风暴阈值(条/分)', dataIndex: 'storm_threshold_per_min', key: 'storm_threshold_per_min', width: 130 },
  ]

  return (
    <CrudTable<RouteConfig, RouteRequest>
      resourceLabel="路由"
      queryKey={['admin', 'routes']}
      fetchList={async () => (await listRoutes()).routes}
      createFn={createRoute}
      updateFn={updateRoute}
      deleteFn={deleteRoute}
      columns={columns}
      renderFields={() => (
        <>
          <Form.Item
            name="priority"
            label="优先级（全局唯一整数，越小越先匹配）"
            rules={[{ required: true, message: '请输入优先级' }]}
          >
            <InputNumber style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="matchers" label="匹配条件" extra="留空时作为全局默认路由。">
            <MatchersEditor />
          </Form.Item>
          <Form.Item name="target_team_id" label="目标团队" rules={[{ required: true, message: '请选择目标团队' }]}>
            <Select options={teamOptions} showSearch optionFilterProp="label" />
          </Form.Item>
          {/* group_mode='window' 与 group_by 互斥（后端 422 兜底，见 internal/api/config.go
              validateRoute）：这里用 shouldUpdate 拿到 formInstance，选中 window 时清空
              并禁用 group_by，避免用户提交一个必然被拒绝的组合。 */}
          <Form.Item noStyle shouldUpdate={(prev, cur) => prev.group_mode !== cur.group_mode}>
            {(formInstance) => {
              const isWindow = formInstance.getFieldValue('group_mode') === 'window'
              return (
                <>
                  <Form.Item name="group_mode" label="分组模式" initialValue="label">
                    <Select
                      options={[
                        { value: 'label', label: '按标签分组' },
                        { value: 'window', label: '窗口全归并' },
                      ]}
                      onChange={(value) => {
                        if (value === 'window') {
                          formInstance.setFieldValue('group_by', [])
                        }
                      }}
                    />
                  </Form.Item>
                  <Form.Item
                    name="group_by"
                    label="分组字段"
                    help={isWindow ? '窗口全归并模式下不使用分组键' : undefined}
                  >
                    <Select mode="tags" tokenSeparators={[',']} open={false} disabled={isWindow} />
                  </Form.Item>
                </>
              )
            }}
          </Form.Item>
          <Form.Item name="group_wait_seconds" label="首次分组等待（秒）" initialValue={5}>
            <InputNumber style={{ width: '100%' }} min={0} />
          </Form.Item>
          <Form.Item name="group_interval_seconds" label="同组通知间隔（秒）" initialValue={60}>
            <InputNumber style={{ width: '100%' }} min={0} />
          </Form.Item>
          <Form.Item name="repeat_interval_seconds" label="重复提醒间隔（秒）" initialValue={3600}>
            <InputNumber style={{ width: '100%' }} min={0} />
          </Form.Item>
          <Form.Item
            name="storm_threshold_per_min"
            label="风暴熔断阈值（条/分钟）"
            initialValue={0}
            help="0 表示关闭熔断"
          >
            <InputNumber style={{ width: '100%' }} min={0} />
          </Form.Item>
        </>
      )}
    />
  )
}
