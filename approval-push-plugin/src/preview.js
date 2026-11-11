/** 通知正文的纯函数：Markdown → 一行摘要。 */

/** Join an assistant message's text blocks into one collapsed line. */
export function textOfMessage(message) {
  if (message === null || typeof message !== 'object') return ''
  const blocks = Array.isArray(message.content) ? message.content : []
  let out = ''
  for (const block of blocks) {
    if (block !== null && typeof block === 'object' && block.type === 'text' && typeof block.text === 'string') {
      out += block.text
    }
  }
  return out.replace(/\s+/g, ' ').trim()
}

/** Ellipsize to maxChars code points. */
export function clipPreview(text, maxChars = 160) {
  if (typeof text !== 'string') return ''
  const chars = Array.from(text)
  if (chars.length <= maxChars) return text
  return `${chars.slice(0, maxChars).join('')}…`
}

/** Strip Markdown syntax, keeping words/numbers. */
export function plainText(markdown) {
  if (typeof markdown !== 'string') return ''
  let out = markdown
  out = out.replace(/```[\s\S]*?```/g, ' ')
  out = out.replace(/`([^`]*)`/g, '$1')
  out = out.replace(/!\[[^\]]*\]\([^)]*\)/g, ' ')
  out = out.replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
  out = out.replace(/^\s{0,3}#{1,6}\s+/gm, '')
  out = out.replace(/^\s{0,3}>\s?/gm, '')
  out = out.replace(/^\s*[-*+]\s+/gm, '')
  out = out.replace(/^\s*\d+\.\s+/gm, '')
  out = out.replace(/^\s*\|?[\s:-]*\|[\s:|-]*$/gm, ' ')
  out = out.replace(/\|/g, ' ')
  out = out.replace(/\*\*([^*]*)\*\*/g, '$1')
  out = out.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1$2')
  out = out.replace(/~~([^~]*)~~/g, '$1')
  return out.replace(/\s+/g, ' ').trim()
}

/** First real paragraph → one line, hard-clipped. */
export function summarizeReply(markdown, maxChars = 60) {
  if (typeof markdown !== 'string') return ''
  const blocks = markdown.split(/\n\s*\n/)
  for (const block of blocks) {
    const plain = plainText(block).replace(/^\s*#+\s*/, '').trim()
    if (plain.length > 0) return clipPreview(plain, maxChars)
  }
  return ''
}
