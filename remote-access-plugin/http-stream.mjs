/** Streaming GET with hard timeout + optional private CA. Returns
 *  {status, body(stream WebReadable)}; callers hash/pipe the body. */
import { get as httpsGet } from 'node:https'
import { get as httpGet } from 'node:http'
import { readFileSync } from 'node:fs'

export function requestUrlStream(url, timeoutMs, caFile, headers) {
  return new Promise((resolve, reject) => {
    const isHttps = url.startsWith('https:')
    const getter = isHttps ? httpsGet : httpGet
    const options = { timeout: timeoutMs, headers: headers || {} }
    if (isHttps && caFile) options.ca = readFileSync(caFile)
    const req = getter(url, options, (res) => {
      resolve({ status: res.statusCode ?? 0, body: res })
    })
    req.on('timeout', () => req.destroy(new Error(`timeout after ${timeoutMs}ms`)))
    req.on('error', (e) => reject(e instanceof Error ? e : new Error(String(e))))
  })
}
