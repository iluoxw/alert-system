import { Fragment } from 'react'

// 只匹配 http/https 协议前缀的 URL。这是安全白名单的核心：javascript:、data:
// 等伪协议不满足 "https?:\/\/" 前缀，根本不会被这条正则捕获，从而在正则层面
// 就杜绝了伪协议 XSS，无需额外的协议黑名单判断。
const URL_PATTERN = /https?:\/\/\S+/g

// URL 尾部常被标点粘连（告警文本里常见 "详情见 https://xxx。" 这种中文句号
// 紧跟 URL 无空格的写法），需要把这些标点剥离归还给相邻文本段，否则渲染出的
// 链接会带上多余字符导致点击跳转 404。覆盖英文 ) , . ; : 与中文 。，；）、。
const TRAILING_PUNCT = /[),.;:。，；）、]+$/

// splitLinkSegments 把一段文本切成 text/link 交替的片段序列，供 LinkifiedText
// 逐段渲染。纯字符串处理，不接触 DOM，可脱离组件单独测试/复用。
//
// 测试意图用例（本仓库前端无 test runner，靠此列表覆盖手工/未来自动化验证）：
//   - 纯文本，不含 URL："请检查磁盘使用率" -> 单个 text 段，内容不变
//   - 单个 URL："详情见 https://a.com/x" -> [text, link]
//   - 多个 URL："见 https://a.com 和 https://b.com" -> [text, link, text, link]
//   - 中文标点收尾："见 https://a.com。" -> URL 不含句号，句号回到 text 段
//   - 英文标点收尾："see https://a.com, thanks." -> 逗号回到 text 段
//   - javascript: 伪协议：不匹配正则，整段原样作为 text，不产生链接
//   - 空串输入："" -> [{ kind: 'text', value: '' }]
// 纯函数与唯一使用它的组件刻意同放一个小文件，避免为一个工具函数拆出额外文件
// oxlint-disable-next-line react/only-export-components
export function splitLinkSegments(text: string): Array<{ kind: 'text' | 'link'; value: string }> {
  if (text === '') {
    return [{ kind: 'text', value: '' }]
  }

  const segments: Array<{ kind: 'text' | 'link'; value: string }> = []

  const pushText = (value: string) => {
    if (value === '') return
    const prev = segments[segments.length - 1]
    if (prev && prev.kind === 'text') {
      prev.value += value
    } else {
      segments.push({ kind: 'text', value })
    }
  }

  let lastIndex = 0
  for (const match of text.matchAll(URL_PATTERN)) {
    const raw = match[0]
    const start = match.index ?? 0
    pushText(text.slice(lastIndex, start))

    const trailingMatch = raw.match(TRAILING_PUNCT)
    const trailing = trailingMatch ? trailingMatch[0] : ''
    const url = trailing ? raw.slice(0, raw.length - trailing.length) : raw

    // url 不可能被削成空串：TRAILING_PUNCT 的字符集里没有 '/'，而
    // "https?:\/\/" 前缀本身以 '/' 收尾，从末尾往回扫的连续标点匹配必然
    // 停在这个 '/' 上，至少留下完整的协议前缀。这里仅作防御性兜底。
    if (url === '') {
      pushText(raw)
    } else {
      segments.push({ kind: 'link', value: url })
      pushText(trailing)
    }

    lastIndex = start + raw.length
  }
  pushText(text.slice(lastIndex))

  return segments.length > 0 ? segments : [{ kind: 'text', value: '' }]
}

// LinkifiedText 把告警 description/annotations 里的裸 URL 渲染成可点击链接。
// 告警内容来自外部 webhook，属于不可信输入，因此全程走 React 元素构造
// （text 段作为普通文本节点，React 会自动转义），绝对不能用
// dangerouslySetInnerHTML —— 否则不可信文本里的 HTML/脚本会被当成标签执行。
export function LinkifiedText({ text }: { text: string }) {
  const segments = splitLinkSegments(text)
  return (
    <>
      {segments.map((segment, index) =>
        segment.kind === 'link' ? (
          <a key={index} href={segment.value} target="_blank" rel="noopener noreferrer">
            {segment.value}
          </a>
        ) : (
          <Fragment key={index}>{segment.value}</Fragment>
        ),
      )}
    </>
  )
}
