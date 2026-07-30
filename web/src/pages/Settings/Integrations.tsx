import { useState } from 'react'
import { Form, Input, Modal, Select, Switch, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { CrudTable } from '../../components/CrudTable'
import type { Integration, IntegrationCreateResponse, Severity } from '../../api/client'
import { createIntegration, deleteIntegration, listIntegrations, updateIntegration } from '../../api/client'

const { Text, Paragraph } = Typography

const severityOptions: { value: Severity; label: string }[] = [
  { value: 'critical', label: '严重' },
  { value: 'warning', label: '警告' },
  { value: 'info', label: '提示' },
]

// IntegrationFormValues 是表单内部形状：config 用 JSON 文本框编辑，提交前解析成
// 对象再传给 client.ts（IntegrationRequest.config 是 unknown）。
interface IntegrationFormValues {
  kind: string
  name: string
  default_severity: Severity
  enabled: boolean
  config?: string
}

function parseConfig(text?: string): unknown {
  if (!text || text.trim() === '') return undefined
  return JSON.parse(text)
}

export function Integrations() {
  const [tokenModal, setTokenModal] = useState<{ name: string; token: string } | null>(null)

  const columns: ColumnsType<Integration> = [
    { title: 'kind', dataIndex: 'kind', key: 'kind', width: 120 },
    { title: '名称', dataIndex: 'name', key: 'name' },
    {
      title: '默认等级',
      dataIndex: 'default_severity',
      key: 'default_severity',
      width: 100,
      render: (v: Severity) => severityOptions.find((o) => o.value === v)?.label ?? v,
    },
    { title: '启用', dataIndex: 'enabled', key: 'enabled', width: 80, render: (v: boolean) => (v ? '是' : '否') },
  ]

  return (
    <div>
      <CrudTable<Integration, IntegrationFormValues>
        resourceLabel="接入"
        queryKey={['admin', 'integrations']}
        fetchList={async () => (await listIntegrations()).integrations}
        createFn={(v) =>
          createIntegration({
            kind: v.kind,
            name: v.name,
            default_severity: v.default_severity,
            enabled: v.enabled,
            config: parseConfig(v.config),
          })
        }
        updateFn={(id, v) =>
          updateIntegration(id, {
            kind: v.kind,
            name: v.name,
            default_severity: v.default_severity,
            enabled: v.enabled,
            config: parseConfig(v.config),
          })
        }
        deleteFn={deleteIntegration}
        columns={columns}
        toFormValues={(record) => ({
          kind: record.kind,
          name: record.name,
          default_severity: record.default_severity,
          enabled: record.enabled,
          config: JSON.stringify(record.config ?? {}, null, 2),
        })}
        onCreateSuccess={(created) => {
          // R1.3：token 明文只在这一次响应里出现，之后 GetByID/List/Update 都拿
          // 不到，必须立即弹窗展示并提示用户保存。
          const resp = created as IntegrationCreateResponse
          setTokenModal({ name: resp.name, token: resp.token })
        }}
        renderFields={() => (
          <>
            <Form.Item name="kind" label="接入类型（kind，如 webhook/feishu）" rules={[{ required: true, message: '请输入接入类型' }]}>
              <Input />
            </Form.Item>
            <Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入名称' }]}>
              <Input />
            </Form.Item>
            <Form.Item
              name="default_severity"
              label="默认等级（信号未携带等级时的兜底）"
              initialValue="warning"
              rules={[{ required: true }]}
            >
              <Select options={severityOptions} />
            </Form.Item>
            <Form.Item name="enabled" label="启用" valuePropName="checked" initialValue={true}>
              <Switch />
            </Form.Item>
            <Form.Item
              name="config"
              label="渠道配置（JSON，具体字段视 kind 而定）"
              rules={[
                {
                  validator: async (_, v) => {
                    if (!v || v.trim() === '') return
                    try {
                      JSON.parse(v)
                    } catch {
                      throw new Error('config 不是合法 JSON')
                    }
                  },
                },
              ]}
            >
              <Input.TextArea rows={4} placeholder="{}" />
            </Form.Item>
          </>
        )}
      />

      <Modal
        title="接入 token（仅显示一次）"
        open={!!tokenModal}
        onOk={() => setTokenModal(null)}
        onCancel={() => setTokenModal(null)}
        cancelButtonProps={{ style: { display: 'none' } }}
      >
        <Paragraph type="danger">此 token 仅在创建时展示一次，请立即复制保存；关闭后无法再次查看。</Paragraph>
        <Paragraph>
          接入「{tokenModal?.name}」的 token：
        </Paragraph>
        <Text code copyable style={{ wordBreak: 'break-all' }}>
          {tokenModal?.token}
        </Text>
      </Modal>
    </div>
  )
}
