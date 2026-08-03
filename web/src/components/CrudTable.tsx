import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert as AntAlert, Button, Empty, Form, Modal, Popconfirm, Space, Table, message } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { PlusOutlined } from '@ant-design/icons'
import { ApiError } from '../api/client'

// CrudTable 是设置页（Settings/*.tsx）共用的通用管理表格：列表查询 + 创建/编辑弹窗
// + 删除确认，各资源只需要传入自己的列定义（columns）与表单字段（renderFields），
// 复用同一套增删改查交互与 React Query 缓存失效逻辑。字段差异较大的资源（如
// escalation-policies 的多级升级链、notification-policies 的按 severity 分层
// steps）仍然通过 renderFields 传入自定义子表单，只是外层的弹窗/校验/提交流程
// 不用每个资源重新写一遍。

interface CrudTableProps<T extends { id: string }, TFormValues extends object> {
  resourceLabel: string // 中文资源名，用于弹窗标题/提示文案，如"团队"
  queryKey: unknown[]
  fetchList: () => Promise<T[]>
  createFn?: (body: TFormValues) => Promise<unknown>
  updateFn?: (id: string, body: TFormValues) => Promise<unknown>
  deleteFn: (id: string) => Promise<void>
  columns: ColumnsType<T>
  // renderFields 返回 Form.Item 列表；editing 为 null 表示当前是创建弹窗（部分
  // 字段如 users 的 password 只在创建时出现，由调用方按 editing 是否为 null 判断）。
  renderFields: (editing: T | null) => ReactNode
  // toFormValues 把列表行数据转换成编辑弹窗的初始表单值；不传则直接用行数据本身。
  toFormValues?: (record: T) => Partial<TFormValues>
  onCreateSuccess?: (created: unknown) => void
  extraToolbar?: ReactNode
  // deleteConfirmTitle 覆盖默认的删除确认文案，供有额外风险提示的资源使用
  // （如 silences 删除会立即恢复通知，R10.5）。
  deleteConfirmTitle?: string
}

export function CrudTable<T extends { id: string }, TFormValues extends object>({
  resourceLabel,
  queryKey,
  fetchList,
  createFn,
  updateFn,
  deleteFn,
  columns,
  renderFields,
  toFormValues,
  onCreateSuccess,
  extraToolbar,
  deleteConfirmTitle,
}: CrudTableProps<T, TFormValues>) {
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()
  const [modalOpen, setModalOpen] = useState(false)
  const [editing, setEditing] = useState<T | null>(null)
  const [form] = Form.useForm<TFormValues>()

  const { data, isLoading, error } = useQuery({ queryKey, queryFn: fetchList })

  const invalidate = () => queryClient.invalidateQueries({ queryKey })

  const createMutation = useMutation({
    mutationFn: (values: TFormValues) => createFn!(values),
    onSuccess: (created) => {
      messageApi.success(`${resourceLabel}创建成功`)
      invalidate()
      setModalOpen(false)
      onCreateSuccess?.(created)
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : `创建${resourceLabel}失败`),
  })

  const updateMutation = useMutation({
    mutationFn: (values: TFormValues) => updateFn!(editing!.id, values),
    onSuccess: () => {
      messageApi.success(`${resourceLabel}更新成功`)
      invalidate()
      setModalOpen(false)
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : `更新${resourceLabel}失败`),
  })

  const deleteMutation = useMutation({
    mutationFn: (id: string) => deleteFn(id),
    onSuccess: () => {
      messageApi.success(`${resourceLabel}已删除`)
      invalidate()
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : `删除${resourceLabel}失败`),
  })

  useEffect(() => {
    if (!modalOpen) return
    if (editing) {
      form.setFieldsValue((toFormValues ? toFormValues(editing) : editing) as TFormValues)
    } else {
      form.resetFields()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [modalOpen, editing])

  const openCreate = () => {
    setEditing(null)
    setModalOpen(true)
  }
  const openEdit = (record: T) => {
    setEditing(record)
    setModalOpen(true)
  }

  const handleSubmit = async () => {
    const values = await form.validateFields()
    if (editing) {
      updateMutation.mutate(values)
    } else {
      createMutation.mutate(values)
    }
  }

  const allColumns: ColumnsType<T> = [
    ...columns,
    {
      title: '操作',
      key: 'actions',
      width: 140,
      render: (_: unknown, record: T) => (
        <Space>
          {updateFn && <a onClick={() => openEdit(record)}>编辑</a>}
          <Popconfirm
            title={deleteConfirmTitle ?? `确认删除该${resourceLabel}？`}
            okText="确认删除"
            cancelText="取消"
            onConfirm={() => deleteMutation.mutate(record.id)}
          >
            <a style={{ color: '#ff4d4f' }}>删除</a>
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <section className="resource-surface">
      {contextHolder}
      <div className="resource-toolbar">
        <Space>{extraToolbar ?? <span className="resource-toolbar__hint">配置变更保存后立即生效</span>}</Space>
        {createFn && (
          <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
            新建{resourceLabel}
          </Button>
        )}
      </div>

      {error && (
        <AntAlert
          type="error"
          message={`加载${resourceLabel}列表失败`}
          description={error instanceof Error ? error.message : String(error)}
          style={{ marginBottom: 16 }}
        />
      )}

      <Table<T>
        rowKey="id"
        columns={allColumns}
        dataSource={data ?? []}
        loading={isLoading}
        pagination={false}
        scroll={{ x: 'max-content' }}
        locale={{
          emptyText: (
            <Empty
              image={Empty.PRESENTED_IMAGE_SIMPLE}
              description={`还没有${resourceLabel}，可从右上角开始创建`}
            />
          ),
        }}
      />

      <Modal
        title={editing ? `编辑${resourceLabel}` : `新建${resourceLabel}`}
        open={modalOpen}
        onCancel={() => setModalOpen(false)}
        onOk={handleSubmit}
        confirmLoading={createMutation.isPending || updateMutation.isPending}
        width={720}
        okText={editing ? '保存修改' : `创建${resourceLabel}`}
        cancelText="取消"
        destroyOnHidden
      >
        <Form<TFormValues> form={form} layout="vertical">
          {renderFields(editing)}
        </Form>
      </Modal>
    </section>
  )
}
