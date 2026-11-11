import test from 'node:test'
import assert from 'node:assert/strict'
import {
  normalizeVersion, compareVersions, isNewer, pickReleaseAssets,
  githubUpdateFromRelease, parseChecksum,
} from '../src/update-source.js'

test('normalizeVersion strips v prefix and suffixes', () => {
  assert.equal(normalizeVersion('v0.1.19'), '0.1.19')
  assert.equal(normalizeVersion('0.1.19-fix2'), '0.1.19')
  assert.equal(normalizeVersion('  '), '')
  assert.equal(normalizeVersion(undefined), '')
})

test('compareVersions handles missing segments as zero', () => {
  assert.equal(compareVersions('0.1.19', '0.1.18'), 1)
  assert.equal(compareVersions('0.1.9', '0.1.10'), -1)
  assert.equal(compareVersions('v0.1.19', '0.1.19'), 0)
  assert.equal(compareVersions('0.2', '0.1.99'), 1)
  assert.equal(isNewer('0.1.19', '0.1.19'), false)
  assert.equal(isNewer('0.1.20', '0.1.19'), true)
})

const release = (tag, assets) => ({
  tag_name: tag, name: `release ${tag}`, body: 'notes', published_at: '2026-11-10T02:00:00Z',
  draft: false, prerelease: false, assets,
})

test('githubUpdateFromRelease maps a newer release with arm64 apk + checksum', () => {
  const r = release('v0.1.20', [
    { name: 'app-arm64-release.apk', browser_download_url: 'https://github.com/x/y/releases/download/v0.1.20/app-arm64-release.apk', size: 123 },
    { name: 'app-arm64-release.apk.sha256', browser_download_url: 'https://github.com/x/y/releases/download/v0.1.20/app-arm64-release.apk.sha256', size: 70 },
  ])
  const u = githubUpdateFromRelease(r, '0.1.19')
  assert.equal(u.version, '0.1.20')
  assert.equal(u.url.endsWith('app-arm64-release.apk'), true)
  assert.equal(u.checksumUrl.endsWith('.sha256'), true)
  assert.equal(u.size, 123)
  assert.equal(u.force, false)
  assert.equal(u.source, 'github')
})

test('same or older release → null (no update)', () => {
  assert.equal(githubUpdateFromRelease(release('v0.1.19', [{ name: 'app-arm64-release.apk', browser_download_url: 'u' }]), '0.1.19'), null)
  assert.equal(githubUpdateFromRelease(release('v0.1.18', [{ name: 'app-arm64-release.apk', browser_download_url: 'u' }]), '0.1.19'), null)
})

test('draft / prerelease → null', () => {
  const r = { ...release('v0.2.0', [{ name: 'app-arm64-release.apk', browser_download_url: 'u' }]), prerelease: true }
  assert.equal(githubUpdateFromRelease(r, '0.1.19'), null)
})

test('release without APK asset throws → caller falls back to relay', () => {
  assert.throws(() => githubUpdateFromRelease(release('v0.2.0', [{ name: 'notes.txt', browser_download_url: 'u' }]), '0.1.19'), /no APK asset/)
})

test('pickReleaseAssets prefers exact arm64 asset, then arm64, then any apk', () => {
  const { apk } = pickReleaseAssets({ assets: [
    { name: 'app-x86_64-release.apk' },
    { name: 'app-arm64-release.apk' },
    { name: 'other-arm64-debug.apk' },
  ] })
  assert.equal(apk.name, 'app-arm64-release.apk')
})

test('parseChecksum extracts the first 64-hex digest', () => {
  const sha = 'fe92949fc5262758354652a264e1c9a94c1ce16b5934ee0e1bb95f2e42118b98'
  assert.equal(parseChecksum(`${sha}  app-arm64-release.apk\n`), sha)
  assert.equal(parseChecksum('not a checksum'), '')
})
