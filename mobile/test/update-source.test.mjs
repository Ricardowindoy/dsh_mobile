import test from 'node:test'
import assert from 'node:assert/strict'
import {
  normalizeVersion, compareVersions, isNewer, pickReleaseAssets,
  githubUpdateFromRelease, giteeUpdateFromRelease, parseChecksum,
  normalizeSource, UPDATE_SOURCES, DEFAULT_UPDATE_SOURCE,
  pickLatestGiteeRelease,
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

test('giteeUpdateFromRelease maps a newer Gitee release (created_at + same asset shape)', () => {
  const r = {
    tag_name: 'v1.0.20', name: 'v1.0.20', body: 'gitee notes',
    created_at: '2026-11-11T01:00:00Z',
    assets: [
      { name: 'app-arm64-release.apk', browser_download_url: 'https://gitee.com/x/y/releases/download/v1.0.20/app-arm64-release.apk', size: 456 },
      { name: 'app-arm64-release.apk.sha256', browser_download_url: 'https://gitee.com/x/y/releases/download/v1.0.20/app-arm64-release.apk.sha256' },
    ],
  }
  const u = giteeUpdateFromRelease(r, '1.0.11')
  assert.equal(u.version, '1.0.20')
  assert.equal(u.source, 'gitee')
  assert.equal(u.releaseAt, '2026-11-11')
  assert.equal(u.size, 456)
  assert.equal(u.checksumUrl.endsWith('.sha256'), true)
})

test('giteeUpdateFromRelease: same/older → null, no apk throws, prerelease → null', () => {
  const mk = (tag, extra = {}) => ({ tag_name: tag, created_at: '2026-11-11T00:00:00Z',
    assets: [{ name: 'app-arm64-release.apk', browser_download_url: 'u' }], ...extra })
  assert.equal(giteeUpdateFromRelease(mk('v1.0.11'), '1.0.11'), null)
  assert.equal(giteeUpdateFromRelease(mk('v1.0.10'), '1.0.11'), null)
  assert.equal(giteeUpdateFromRelease(mk('v1.0.12', { prerelease: true }), '1.0.11'), null)
  assert.throws(() => giteeUpdateFromRelease(
    { tag_name: 'v2.0.0', created_at: 'x', assets: [{ name: 'notes.txt', browser_download_url: 'u' }] }, '1.0.11'),
    /no APK asset/)
})

test('normalizeSource keeps valid sources, falls back to default for junk', () => {
  for (const s of UPDATE_SOURCES) assert.equal(normalizeSource(s), s)
  assert.equal(normalizeSource('  GITHUB '), 'github')
  assert.equal(normalizeSource('gitlab'), DEFAULT_UPDATE_SOURCE)
  assert.equal(normalizeSource(''), DEFAULT_UPDATE_SOURCE)
  assert.equal(normalizeSource(null), DEFAULT_UPDATE_SOURCE)
  assert.equal(normalizeSource(undefined), DEFAULT_UPDATE_SOURCE)
})

// ── v1.0.17 regression: Gitee lists releases in ASCENDING order ─────────────
// Taking arr[0] returned the oldest tag, so "download from Gitee" reported
// "already up to date" forever. Selection must be by version, not position.
const giteeAsset = (tag) => ([
  { name: 'app-arm64-release.apk', size: null, browser_download_url: `https://gitee.com/n/r/releases/download/${tag}/app-arm64-release.apk` },
  { name: 'app-arm64-release.apk.sha256', size: null, browser_download_url: `https://gitee.com/n/r/releases/download/${tag}/app-arm64-release.apk.sha256` },
])
const giteeList = (tags) => tags.map((tag) => ({ tag_name: tag, name: tag, created_at: '2026-11-11T00:00:00+08:00', prerelease: false, assets: giteeAsset(tag) }))

test('pickLatestGiteeRelease picks the newest even when Gitee returns ascending order', () => {
  const ascending = giteeList(['v1.0.13', 'v1.0.14', 'v1.0.15', 'v1.0.16'])
  assert.equal(pickLatestGiteeRelease(ascending).tag_name, 'v1.0.16')
})

test('pickLatestGiteeRelease is order-independent (descending / shuffled)', () => {
  const descending = giteeList(['v1.0.16', 'v1.0.15', 'v1.0.14'])
  assert.equal(pickLatestGiteeRelease(descending).tag_name, 'v1.0.16')
  const shuffled = giteeList(['v1.0.14', 'v1.0.16', 'v1.0.13', 'v1.0.15'])
  assert.equal(pickLatestGiteeRelease(shuffled).tag_name, 'v1.0.16')
})

test('pickLatestGiteeRelease tolerates junk entries and empty lists', () => {
  assert.equal(pickLatestGiteeRelease([]), null)
  assert.equal(pickLatestGiteeRelease(null), null)
  assert.equal(pickLatestGiteeRelease([{ tag_name: '' }, null, { name: 'no-version' }]), null)
  const mixed = [{ tag_name: '' }, { tag_name: 'v1.0.15', assets: giteeAsset('v1.0.15') }, null]
  assert.equal(pickLatestGiteeRelease(mixed).tag_name, 'v1.0.15')
})

test('regression: ascending list at v1.0.15 yields the v1.0.16 update', () => {
  const ascending = giteeList(['v1.0.13', 'v1.0.14', 'v1.0.15', 'v1.0.16'])
  // The old behaviour (arr[0]) is the bug this pins:
  assert.equal(giteeUpdateFromRelease(ascending[0], '1.0.15'), null)
  const update = giteeUpdateFromRelease(pickLatestGiteeRelease(ascending), '1.0.15')
  assert.equal(update.version, '1.0.16')
  assert.equal(update.source, 'gitee')
  assert.equal(update.url.endsWith('app-arm64-release.apk'), true)
})
