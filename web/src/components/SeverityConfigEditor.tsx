import { useEffect } from 'react'
import { Button, Card, Checkbox, InputNumber, Typography } from 'antd'
import type { Severity, SeverityConfig } from '../api/client'

const { Text } = Typography

// SeverityConfigEditor 是升级策略表单里的 severity 覆写编辑器（ops-enhancements
// 任务 11，对应后端 R1 覆写口径 validateSeverityConfig）：每个 severity 一块，可
// 勾选"跳过即时升级"（对应 skip，勾选后禁用该 severity 的覆写输入——skip 优先，
// 编辑覆写值此时无意义），或按级别覆写等待时长（对应 overrides）。
//
// overrides[severity] 在后端是从第 0 级开始的定长前缀数组（len(waits) 可短于
// levels 但不能更长，位置对应 levels[i]，缺位回退默认节奏），不支持"跳过中间
// 级别单独覆写后面级别"这种稀疏组合。所以这里把每个 severity 渲染成 levelCount
// 个格子，一旦某格留空，后续格子会被禁用并在下次编辑时一并从存储里截断，避免
// 用户以为自己覆写了后面的级别、实际上后端会拒绝或悄悄丢弃。
//
// 表单里流转的值统一是秒（与 EscalationLevel.wait_seconds 同口径），仅在渲染/
// 输入时换算成分钟并保留两位小数——两位小数下"分钟→秒"换算的最大误差是
// 0.3 秒，小于四舍五入的判定阈值 0.5 秒，所以任意整数秒值都能在"秒→分钟→秒"
// 往返后精确还原，不会因为换算丢精度。

const severities: Severity[] = ['critical', 'warning', 'info']
const severityLabel: Record<Severity, string> = { critical: '严重', warning: '警告', info: '提示' }

// 建议值来自生产验证参数：critical 走 3min/10min 快速升级，warning 走
// 60min/120min 更长缓冲，info 默认跳过即时升级（噪音级别，无需人员响应）。
const suggestedWaitsSeconds: Record<Severity, number[]> = {
  critical: [180, 600],
  warning: [3600, 7200],
  info: [],
}
const suggestedSkip: Severity[] = ['info']

const secondsToMinutes = (seconds: number) => Math.round((seconds / 60) * 100) / 100
const minutesToSeconds = (minutes: number) => Math.round(minutes * 60)

// 把 overrides[severity]（后端定长前缀数组）铺开成 levelCount 个格子，缺位补 null。
function cellsFor(overrides: Partial<Record<Severity, number[]>>, severity: Severity, levelCount: number): (number | null)[] {
  const waits = overrides[severity] ?? []
  return Array.from({ length: levelCount }, (_, i) => (i < waits.length ? waits[i] : null))
}

// 从格子还原出前缀数组：遇到第一个空格即截断，之后的格子即便有值也一并丢弃
// （保持"定长前缀"这一后端不变量，见文件头注释）。
function truncateToPrefix(cells: (number | null)[]): number[] {
  const result: number[] = []
  for (const c of cells) {
    if (c == null) break
    result.push(c)
  }
  return result
}

interface SeverityConfigEditorProps {
  value?: SeverityConfig
  onChange?: (value: SeverityConfig) => void
  levelCount: number
  // showSuggestedFill 仅在新建策略时传 true——编辑已有策略时不提供"一键覆盖"按钮，
  // 避免误覆盖已生效的配置。
  showSuggestedFill?: boolean
}

export function SeverityConfigEditor({ value, onChange, levelCount, showSuggestedFill }: SeverityConfigEditorProps) {
  const overrides = value?.overrides ?? {}
  const skip = value?.skip ?? []

  const emit = (next: SeverityConfig) => onChange?.(next)

  // levels 数组变短时，已有覆写可能超出新的 levelCount（后端会拒绝），这里主动
  // 截断保持"overrides 长度不超过 levels 长度"这一后端校验不变量。
  useEffect(() => {
    let changed = false
    const trimmed: Partial<Record<Severity, number[]>> = {}
    for (const severity of severities) {
      const waits = overrides[severity]
      if (!waits) continue
      if (waits.length > levelCount) {
        changed = true
        if (levelCount > 0) trimmed[severity] = waits.slice(0, levelCount)
      } else {
        trimmed[severity] = waits
      }
    }
    if (changed) emit({ overrides: trimmed, skip })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [levelCount])

  const setSkip = (severity: Severity, checked: boolean) => {
    const nextSkip = checked ? [...skip.filter((s) => s !== severity), severity] : skip.filter((s) => s !== severity)
    emit({ overrides, skip: nextSkip })
  }

  const setWait = (severity: Severity, index: number, minutes: number | null) => {
    const cells = cellsFor(overrides, severity, levelCount)
    cells[index] = minutes == null ? null : minutesToSeconds(minutes)
    const prefix = truncateToPrefix(cells)
    const nextOverrides = { ...overrides }
    if (prefix.length > 0) {
      nextOverrides[severity] = prefix
    } else {
      delete nextOverrides[severity]
    }
    emit({ overrides: nextOverrides, skip })
  }

  const fillSuggested = () => {
    const nextOverrides: Partial<Record<Severity, number[]>> = {}
    for (const severity of severities) {
      const waits = suggestedWaitsSeconds[severity].slice(0, levelCount)
      if (waits.length > 0) nextOverrides[severity] = waits
    }
    emit({ overrides: nextOverrides, skip: [...suggestedSkip] })
  }

  return (
    <div>
      {showSuggestedFill && (
        <Button size="small" onClick={fillSuggested} style={{ marginBottom: 8 }}>
          使用建议节奏
        </Button>
      )}
      {severities.map((severity) => {
        const isSkipped = skip.includes(severity)
        const cells = cellsFor(overrides, severity, levelCount)
        const firstBlank = cells.findIndex((c) => c == null)
        return (
          <Card key={severity} size="small" title={`${severityLabel[severity]} 级别`} style={{ marginBottom: 8 }}>
            <Checkbox checked={isSkipped} onChange={(e) => setSkip(severity, e.target.checked)}>
              跳过即时升级
            </Checkbox>
            {levelCount > 0 && (
              <div style={{ marginTop: 8 }}>
                <Text type="secondary" style={{ display: 'block', marginBottom: 4, fontSize: 12 }}>
                  按级别覆写等待时长（分钟），某级留空则该级及之后各级使用默认节奏
                </Text>
                <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                  {cells.map((cell, i) => (
                    <div className="severity-wait-field" key={i}>
                      <span>L{i + 1}</span>
                      <InputNumber
                        min={0}
                        step={0.5}
                        precision={2}
                        placeholder={firstBlank !== -1 && i > firstBlank ? '需先填上一级' : '默认'}
                        disabled={isSkipped || (firstBlank !== -1 && i > firstBlank)}
                        value={cell == null ? null : secondsToMinutes(cell)}
                        onChange={(minutes) => setWait(severity, i, minutes)}
                      />
                    </div>
                  ))}
                </div>
              </div>
            )}
          </Card>
        )
      })}
    </div>
  )
}
