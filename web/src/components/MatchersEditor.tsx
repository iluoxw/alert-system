import { Button, Select, Space, Input } from 'antd'
import { MinusCircleOutlined, PlusOutlined } from '@ant-design/icons'
import type { MatchOp, MatcherSet } from '../api/client'

// MatchersEditor 是 routes/notification-policies/silences 三处表单共用的 matcher
// 行编辑器（name/op/value 三元组数组），对应 domain.MatcherSet 的 AND 语义。
// 与 AlertList.tsx 的标签过滤行是同一交互模式（增删行），这里额外加了 op 选择器。

const opOptions: { value: MatchOp; label: string }[] = [
  { value: '=', label: '=（等于）' },
  { value: '!=', label: '!=（不等于）' },
  { value: '=~', label: '=~（正则匹配）' },
  { value: '!~', label: '!~（正则不匹配）' },
]

interface MatchersEditorProps {
  value?: MatcherSet
  onChange?: (value: MatcherSet) => void
}

export function MatchersEditor({ value, onChange }: MatchersEditorProps) {
  const rows = value ?? []

  const emit = (next: MatcherSet) => onChange?.(next)

  const addRow = () => emit([...rows, { name: '', op: '=', value: '' }])
  const removeRow = (index: number) => emit(rows.filter((_, i) => i !== index))
  const updateRow = (index: number, patch: Partial<MatcherSet[number]>) =>
    emit(rows.map((row, i) => (i === index ? { ...row, ...patch } : row)))

  return (
    <Space direction="vertical" size={4} style={{ width: '100%' }}>
      {rows.map((row, index) => (
        <Space key={index}>
          <Input
            placeholder="标签名"
            style={{ width: 140 }}
            value={row.name}
            onChange={(e) => updateRow(index, { name: e.target.value })}
          />
          <Select<MatchOp>
            style={{ width: 130 }}
            value={row.op}
            options={opOptions}
            onChange={(op) => updateRow(index, { op })}
          />
          <Input
            placeholder="标签值/正则"
            style={{ width: 160 }}
            value={row.value}
            onChange={(e) => updateRow(index, { value: e.target.value })}
          />
          <MinusCircleOutlined onClick={() => removeRow(index)} />
        </Space>
      ))}
      <Button type="dashed" icon={<PlusOutlined />} onClick={addRow}>
        添加 matcher
      </Button>
    </Space>
  )
}
