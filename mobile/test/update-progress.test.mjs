import test from 'node:test'
import assert from 'node:assert/strict'
import { pctOf, formatBytes, progressLabel } from '../src/update-progress.js'

test('pctOf floors, clamps, and refuses indeterminate totals', () => {
  assert.equal(pctOf(0, 1000), 0)
  assert.equal(pctOf(500, 1000), 50)
  assert.equal(pctOf(999, 1000), 99)   // floor：没下完就不显示 100
  assert.equal(pctOf(1000, 1000), 100)
  assert.equal(pctOf(1500, 1000), 100) // 超收钳到 100
  assert.equal(pctOf(0, 0), null)
  assert.equal(pctOf(0, null), null)
  assert.equal(pctOf(0, undefined), null)
  assert.equal(pctOf(-1, 100), null)
  assert.equal(pctOf(NaN, 100), null)
})

test('formatBytes picks KB below 1MB and MB above', () => {
  assert.equal(formatBytes(0), '0.0 KB')
  assert.equal(formatBytes(512), '0.5 KB')
  assert.equal(formatBytes(1048576), '1.0 MB')
  assert.equal(formatBytes(30945715), '29.5 MB')
  assert.equal(formatBytes(-1), '')
  assert.equal(formatBytes(undefined), '')
})

test('progressLabel composes percent and byte facts', () => {
  assert.equal(progressLabel(42, 12345678, 30945715), '42% · 11.8 MB / 29.5 MB')
  assert.equal(progressLabel(null, 1048576, null), '1.0 MB') // 不定态只报已收
  assert.equal(progressLabel(null, undefined, null), '')
})
