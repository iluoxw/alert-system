import { Select } from 'antd'
import type { SelectProps } from 'antd'

const intlWithTimeZones = Intl as typeof Intl & {
  supportedValuesOf?: (key: 'timeZone') => string[]
}

const fallbackTimeZones = [
  'UTC',
  'Asia/Shanghai',
  'Asia/Hong_Kong',
  'Asia/Tokyo',
  'Asia/Singapore',
  'Europe/London',
  'Europe/Berlin',
  'America/Los_Angeles',
  'America/New_York',
]

const timeZones = Array.from(new Set([
  'UTC',
  ...(intlWithTimeZones.supportedValuesOf?.('timeZone') ?? fallbackTimeZones),
])).sort((left, right) => left.localeCompare(right))

const options = timeZones.map((timeZone) => ({ value: timeZone, label: timeZone }))

export function TimezoneSelect(props: Omit<SelectProps<string>, 'options'>) {
  return (
    <Select<string>
      showSearch
      optionFilterProp="label"
      placeholder="选择 IANA 时区"
      options={options}
      {...props}
    />
  )
}
