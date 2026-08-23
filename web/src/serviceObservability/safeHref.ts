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

// WHATWG URL 的 protocol 带尾随冒号。
const httpProtocols = ['http:', 'https:'] as const

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

// isAbsoluteHTTP 判断是否是 **host 非空**的绝对 http/https URL。
//
// 判定必须真正解析 URL，不能只看"scheme 前缀之后还有字符"：那种前缀检查会放过一整类
// 没有主机名或根本无法解析的值（`https://user@/path`、`https://:443/path`、
// `https://[::1`、`https://exa mple.com`、`https://evil.example:bad/path`），而浏览器
// 自己的 URL 解析全都拒绝它们——于是 UI 会渲染出一个点开后行为未定义的链接。
//
// 这里直接用 WHATWG `URL`（浏览器与 Node 同一套语义），并要求 protocol 是 http/https、
// hostname 非空。hostname 是关键：它剥掉 userinfo 与端口，因此"authority 文本非空但没有
// 主机名"的值不会被误判成合法。后端 internal/safelink 用 net/url 做同一件事，两侧共用
// testdata/link_policy_cases.json 这张表来机械证明口径一致。
// emptyAuthorityPattern 匹配 `scheme://` 之后直接是 `/`、`?`、`#` 或字符串结尾的形态，
// 也就是"authority 为空"。
//
// 这一条前置检查是为了消除一个真实的解析器分歧：WHATWG URL 会把 `http:///runbooks`
// **规范化**成 `http://runbooks/`（把路径的第一段当成主机名！），而 Go 的 net/url 认为
// 它没有 host。也就是说同一个字符串在浏览器里会跳到外站 `runbooks`，在服务端校验里却
// 是"无主机"。一个目标取决于"问哪个解析器"的值绝不能变成可点击链接，因此两侧都显式
// 拒绝它，而不是各自跟随自己的解析器。
const emptyAuthorityPattern = /^[A-Za-z][A-Za-z0-9+.-]*:\/\/(?:[/?#]|$)/

function isAbsoluteHTTP(value: string): boolean {
  if (emptyAuthorityPattern.test(value)) return false
  let parsed: URL
  try {
    parsed = new URL(value)
  } catch {
    // 无法解析（含裸主机与 malformed authority）一律不合法。
    return false
  }
  if (!httpProtocols.includes(parsed.protocol.toLowerCase() as (typeof httpProtocols)[number])) {
    return false
  }
  return parsed.hostname !== ''
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
