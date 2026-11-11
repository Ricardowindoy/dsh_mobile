/**
 * QR pairing helpers for the remote-access plugin (host side).
 *
 * The QR payload is a JSON document the phone App understands:
 *   {"v":1,"relay":"https://…","ca":"ip"|"domain"|"none","code":"<one-time>"}
 * It carries the ONE-TIME pairing code only — never the management token and
 * never a device token. The relay burns the code on first use.
 *
 * QR rendering uses the vendored MIT-licensed qrcode-generator (Kazuhiko
 * Arase, qrcode-gen.cjs). Server-side SVG keeps the client bundle free of any
 * new dependency.
 */

import { createRequire } from 'node:module'

const require = createRequire(import.meta.url)
const qrcode = require('./qrcode-gen.cjs')

/** Build the QR payload document. Throws on unusable input rather than
 *  emitting a code the phone cannot act on. */
export function pairingPayload({ relay, ca, code }) {
  const base = String(relay || '').replace(/\/+$/, '')
  let u
  try { u = new URL(base) } catch { throw new Error('relay URL is not a valid URL') }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') throw new Error('relay URL must be http(s)')
  if (typeof code !== 'string' || code.length < 16) throw new Error('pairing code missing or too short')
  const caKind = ca === 'ip' || ca === 'domain' ? ca : 'none'
  return { v: 1, relay: base, ca: caKind, code }
}

/** Render a payload as a standalone SVG string (scalable, no canvas). */
export function pairingSvg(payload, cellSize = 5, margin = 4) {
  const qr = qrcode(0, 'M')
  qr.addData(JSON.stringify(payload))
  qr.make()
  return qr.createSvgTag(cellSize, margin)
}

/** Classify which trust anchor a relay entrance needs, from its URL shape:
 *  an IP literal entrance uses the APK-embedded private CA ("ip"); a hostname
 *  entrance uses the public LE chain ("domain"). The App uses this to decide
 *  whether the entrance matches its built-in anchors. */
export function caKindOf(relayUrl) {
  try {
    const u = new URL(relayUrl)
    if (/^\d+\.\d+\.\d+\.\d+$/.test(u.hostname) || u.hostname.includes(':')) return 'ip'
    return 'domain'
  } catch {
    return 'none'
  }
}