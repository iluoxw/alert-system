// 运营页面可点击入口的链接安全策略（前端侧）。
//
// 它与后端 internal/safelink 是**同一套** allowlist：只允许 alert-system 内部单斜杠相对
// 路径，或 host 非空的绝对 http(s) URL。两侧任何一侧放宽都会让"服务端拒绝写入、前端却
// 渲染出可点击链接"这类不一致重新出现，因此静态验证器会断言两份实现的策略描述一致。
//
// 被拒绝的形态与后端逐条相同：`javascript:`、`data:`、`vbscript:`、`mailto:`、任何自定义
// scheme、裸主机（`example.com/x`）、scheme-relative（`//host`）、控制字符，以及用反斜杠
// 伪造 authority 的写法（`/\host`、`\\host`）。
//
// 需要显式记录的口径：拒绝 `mailto:`、自定义 scheme、裸主机与 scheme-relative 是对 V1 的
// **有意安全收紧**。运营页面的链接会被值班人在事故中直接点开；EvidenceRef 的 locator 更是
// 来自外部系统的不可信输入。
//
// 本模块保持纯净：不 import fixture、不 import API client、不碰 `window`，因此可以被
// Node 直接编译执行，回归测试跑的是真实实现而不是它的副本。

const httpSchemes = ['http://', 'https://'] as const

/** normalizeHref 只去掉外层空白：内部空白与控制字符不是格式问题，而是非法输入。 */
export function normalizeHref(raw: string | undefined | null): string {
  return (raw ?? '').trim()
}

// containsControlCharacters 与后端同口径：C0 控制字符与 DEL 一律拒绝。控制字符可以把
// 链接拆行、伪造显示文本，或绕过只看前缀的过滤。
function containsControlCharacters(value: string): boolean {
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0
    if (code < 0x20 || code === 0x7f) return true
  }
  return false
}

function isAbsoluteHTTP(value: string): boolean {
  const lowered = value.toLowerCase()
  for (const scheme of httpSchemes) {
    if (!lowered.startsWith(scheme)) continue
    const authority = value.slice(scheme.length)
    if (authority === '') return false
    // host 必须非空：`http:///path` 的 authority 以 `/` 开头，等于没有 host。
    if (authority.startsWith('/') || authority.startsWith('?') || authority.startsWith('#')) {
      return false
    }
    return true
  }
  return false
}

/**
 * isSafeHref 判断一个链接是否可以渲染成可点击入口。
 *
 * 空值返回 false：调用方据此渲染"不可打开"状态，而不是一个指向当前页面的空 href。
 */
export function isSafeHref(raw: string | undefined | null): boolean {
  const value = normalizeHref(raw)
  if (value === '') return false
  if (containsControlCharacters(value)) return false
  // 反斜杠一律拒绝：浏览器把 `/\host` 与 `\\host` 当成 authority 前缀，于是"内部路径"
  // 会跳到外站。合法的内部路径与 http(s) URL 都不需要反斜杠。
  if (value.includes('\\')) return false
  // scheme-relative：继承当前页面协议后跳到任意外站主机。
  if (value.startsWith('//')) return false
  if (value.startsWith('/')) return true
  return isAbsoluteHTTP(value)
}

/**
 * safeHref 返回可安全渲染的链接；不安全时返回 undefined。
 *
 * 调用方必须据此**省略** href/anchor，而不是渲染一个空 href——空 href 会跳回当前页面，
 * 让"这条证据打不开"看起来像"点了没反应"。它也绝不"修正"一个不安全的链接：猜一个 URL
 * 比不给链接更危险。
 */
export function safeHref(raw: string | undefined | null): string | undefined {
  return isSafeHref(raw) ? normalizeHref(raw) : undefined
}
