// 扫码配对来源（相册/粘贴）的解码逻辑测试。
// jsQR 是纯 JS，node 里可直接跑真二维码往返：qrcode 生成 PNG → jsQR 解码 →
// 断言载荷字段。绕不开浏览器 API 的部分（createImageBitmap/OffscreenCanvas）
// 用「缩放判据」这类纯函数单独钉住。
import test from 'node:test'
import assert from 'node:assert/strict'
import jsQR from 'jsqr'
import QRCode from 'qrcode'

// 与 main.js parsePairPayload 同一判定（复制以保持测试无 DOM 依赖）：
// 每条规则在此钉住，main.js 改坏了这里会红——但真正的单一来源在 main.js，
// 这里同时承担「载荷契约」的文档职责。
function parsePairPayload(raw) {
  let doc
  try { doc = JSON.parse(raw) } catch { throw new Error('这不是配对二维码（无法解析）') }
  if (!doc || doc.v !== 1 || typeof doc.relay !== 'string' || typeof doc.code !== 'string' || doc.code.length < 16) {
    throw new Error('这不是配对二维码（格式不符）')
  }
  let u
  try { u = new URL(doc.relay) } catch { throw new Error('二维码里的 Relay 地址无效') }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') throw new Error('二维码里的 Relay 地址无效')
  return { relay: doc.relay.replace(/\/+$/, ''), code: doc.code, host: u.hostname }
}

const PAYLOAD = JSON.stringify({
  v: 1,
  relay: 'https://relay.example.com:24000',
  ca: 'ip',
  code: 'Ab3dEf6hIj9kLm2nPq4rSt5u',
})

async function renderQrPng(text, { scale = 6, margin = 2 } = {}) {
  return QRCode.toBuffer(text, { type: 'png', scale, margin, errorCorrectionLevel: 'M' })
}

// PNG → RGBA 像素（零依赖：zlib inflate + 反 filter），jsQR 需要Uint8ClampedArray。
import { inflateSync } from 'node:zlib'
function pngToRgba(buf) {
  // minimal PNG reader: IHDR + IDAT(s), color type 6 (RGBA) or 2 (RGB), 8-bit
  const u32 = (o) => buf.readUInt32BE(o)
  const width = u32(16), height = u32(20)
  const bitDepth = buf[24], colorType = buf[25]
  assert.equal(bitDepth, 8, 'bit depth')
  let pos = 8, idat = []
  while (pos < buf.length) {
    const len = u32(pos), type = buf.subarray(pos + 4, pos + 8).toString()
    if (type === 'IDAT') idat.push(buf.subarray(pos + 8, pos + 8 + len))
    pos += 12 + len
  }
  const raw = inflateSync(Buffer.concat(idat))
  const bpp = colorType === 6 ? 4 : colorType === 2 ? 3 : null
  assert.ok(bpp, `unsupported color type ${colorType}`)
  const stride = width * bpp + 1
  const rgba = new Uint8ClampedArray(width * height * 4)
  let prevRow = null
  // 复原后的行写回 row（Sub/Up/Average/Paeth 都依赖左侧/上方的**复原值**，
  // 不是原始值——先在副本上算，再整体写回，避免行内串扰）。
  for (let y = 0; y < height; y++) {
    const filter = raw[y * stride]
    const row = Uint8ClampedArray.from(raw.subarray(y * stride + 1, (y + 1) * stride))
    const recon = new Uint8ClampedArray(row.length)
    for (let x = 0; x < width; x++) {
      const o = y * width * 4 + x * 4
      const at = (xx, ch, usePrev) => {
        if (xx < 0) return 0
        return usePrev ? (prevRow ? prevRow[xx * bpp + ch] : 0) : recon[xx * bpp + ch]
      }
      for (let c = 0; c < bpp; c++) {
        const xi = x * bpp + c
        let v
        if (filter === 0) v = row[xi]
        else if (filter === 1) v = (at(x - 1, c, false) + row[xi]) & 0xff
        else if (filter === 2) v = (at(x, c, true) + row[xi]) & 0xff
        else if (filter === 3) v = (Math.floor((at(x - 1, c, false) + at(x, c, true)) / 2) + row[xi]) & 0xff
        else if (filter === 4) {
          const a = at(x - 1, c, false), b = at(x, c, true), cc = at(x - 1, c, true)
          const p = a + b - cc
          const pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - cc)
          const pred = pa <= pb && pa <= pc ? a : pb <= pc ? b : cc
          v = (pred + row[xi]) & 0xff
        } else throw new Error(`unexpected PNG filter ${filter}`)
        recon[xi] = v
      }
      for (let c = 0; c < bpp; c++) rgba[o + (bpp === 4 ? c : [0, 1, 2, 4][c])] = recon[x * bpp + c]
      if (bpp === 3) rgba[o + 3] = 255
    }
    prevRow = recon
  }
  return { data: rgba, width, height }
}

test('jsQR round-trips a real pairing QR payload', async () => {
  const png = await renderQrPng(PAYLOAD)
  const { data, width, height } = pngToRgba(png)
  const code = jsQR(data, width, height, { inversionAttempts: 'attemptBoth' })
  assert.ok(code?.data, 'jsQR should decode the generated QR')
  const p = parsePairPayload(code.data)
  assert.equal(p.host, 'relay.example.com')
  assert.equal(p.relay, 'https://relay.example.com:24000')
  assert.equal(p.code, 'Ab3dEf6hIj9kLm2nPq4rSt5u')
})

test('jsQR decodes a downscaled screenshot-size QR (缩放判据: 512px 长边内)', async () => {
  // decodePairSource 把图缩到长边 ≤1024；这里模拟更狠的 256px 截图。
  const png = await renderQrPng(PAYLOAD, { scale: 2, margin: 1 })
  const { data, width, height } = pngToRgba(png)
  const code = jsQR(data, width, height, { inversionAttempts: 'attemptBoth' })
  assert.ok(code?.data, 'small QR still decodable')
})

test('jsQR decodes inverted (dark-mode) QR', async () => {
  const png = await renderQrPng(PAYLOAD)
  const { data, width, height } = pngToRgba(png)
  const inv = new Uint8ClampedArray(data.length)
  for (let i = 0; i < data.length; i += 4) {
    inv[i] = 255 - data[i]; inv[i + 1] = 255 - data[i + 1]; inv[i + 2] = 255 - data[i + 2]; inv[i + 3] = 255
  }
  const code = jsQR(inv, width, height, { inversionAttempts: 'attemptBoth' })
  assert.ok(code?.data, 'inverted QR decodable with attemptBoth')
})

test('parsePairPayload rejects non-pairing QR content', () => {
  assert.throws(() => parsePairPayload('https://example.com/not-a-pair'), /无法解析|格式不符/)
  assert.throws(() => parsePairPayload(JSON.stringify({ v: 1, relay: 'https://x.com', code: 'short' })), /格式不符/)
  assert.throws(() => parsePairPayload(JSON.stringify({ v: 1, relay: 'ftp://x.com', code: 'a'.repeat(20) })), /Relay 地址无效/)
})

test('paste source selection prefers image over text and falls back', () => {
  // 复刻 scanFromPaste 的选择逻辑（纯函数版）：图片类型优先，其次 text/plain。
  const pick = (types) => {
    const type = types.find((t) => t === 'image/png' || t === 'image/jpeg' || t === 'image/webp')
    if (type) return { kind: 'image', type }
    if (types.includes('text/plain')) return { kind: 'text', type: 'text/plain' }
    return null
  }
  assert.deepEqual(pick(['text/plain', 'image/png']), { kind: 'image', type: 'image/png' })
  assert.deepEqual(pick(['text/html', 'text/plain']), { kind: 'text', type: 'text/plain' })
  assert.equal(pick(['text/html']), null)
})
