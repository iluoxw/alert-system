import { DatePicker, Form, Input, Tag } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import dayjs from 'dayjs'
import { CrudTable } from '../components/CrudTable'
import { MatchersEditor } from '../components/MatchersEditor'
import type { MatcherSet, Silence } from '../api/client'
import { createSilence, deleteSilence, listSilences } from '../api/client'

const { RangePicker } = DatePicker

// SilenceFormValues 是创建表单在页面内部使用的形状：时间窗用 AntD RangePicker
// 产出的 dayjs 二元组，提交前转换成 SilenceRequest 需要的 ISO 字符串。
interface SilenceFormValues {
  matchers: MatcherSet
  range: [dayjs.Dayjs, dayjs.Dayjs]
  comment?: string
}

export function Silences() {
  const columns: ColumnsType<Silence> = [
    {
      title: '状态',
      key: 'active',
      width: 90,
      render: (_, record) => {
        const active = dayjs().isAfter(record.starts_at) && dayjs().isBefore(record.ends_at)
        return active ? <Tag color="green">生效中</Tag> : <Tag>已过期</Tag>
      },
    },
    {
      title: 'matchers',
      dataIndex: 'matchers',
      key: 'matchers',
      render: (matchers: MatcherSet) => matchers.map((m) => `${m.name}${m.op}${m.value}`).join(', '),
    },
    {
      title: '生效时间窗',
      key: 'window',
      render: (_, record) =>
        `${new Date(record.starts_at).toLocaleString()} ~ ${new Date(record.ends_at).toLocaleString()}`,
    },
    { title: '创建人', dataIndex: 'created_by', key: 'created_by', width: 120 },
    { title: '备注', dataIndex: 'comment', key: 'comment', ellipsis: true },
  ]

  return (
    <CrudTable<Silence, SilenceFormValues>
      resourceLabel="静默"
      queryKey={['silences']}
      fetchList={async () => (await listSilences()).silences}
      createFn={(values) =>
        createSilence({
          matchers: values.matchers,
          starts_at: values.range[0].toISOString(),
          ends_at: values.range[1].toISOString(),
          comment: values.comment,
        })
      }
      // R10.5：删除静默会在同一事务内清理告警的静默引用，立即恢复通知——确认框
      // 文案必须明确提示这一后果，而不是用 CrudTable 的通用"确认删除"文案。
      deleteFn={deleteSilence}
      deleteConfirmTitle="删除后立即恢复相关告警通知，确认删除该静默？"
      columns={columns}
      renderFields={() => (
        <>
          <Form.Item
            name="matchers"
            label="matchers（命中该规则的告警会被静默，不能为空）"
            rules={[{ required: true, validator: async (_, v: MatcherSet) => {
              if (!v || v.length === 0) throw new Error('matchers 不能为空')
            } }]}
          >
            <MatchersEditor />
          </Form.Item>
          <Form.Item name="range" label="生效时间窗" rules={[{ required: true, message: '请选择生效时间窗' }]}>
            <RangePicker showTime style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="comment" label="备注">
            <Input.TextArea rows={2} />
          </Form.Item>
        </>
      )}
    />
  )
}
