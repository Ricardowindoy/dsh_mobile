# CI release build — how it works & one-time setup

Workflow: [`.github/workflows/release-build.yml`](release-build.yml)
Trigger: push a tag `v*` (recommended) or run it manually from the Actions tab.

## What gets built

| job | runner | artifacts |
| --- | --- | --- |
| `android` | ubuntu-latest | signed release **APK** (`app-*-release.apk`) + **AAB** |
| `windows` | windows-latest | NSIS installer `*-setup.exe` |
| `linux` | ubuntu-22.04 | `*.deb` + `*.rpm` + `*.AppImage` |
| `macos` | macos-latest | universal `*.dmg` (arm64 + x86_64) |
| `ios` | macos-latest | unsigned `*.ipa` (arm64; signed build needs Apple certs) |
| `tunnel` | ubuntu-latest | `cmd/tunnel` for linux{amd64,arm64}, darwin/arm64, windows/amd64 |
| `manifest` | ubuntu-latest | `release/manifest.json` + **GitHub Release** with all of the above |

`manifest.json` has the exact shape `scripts/release.sh` writes, so uploading
the `release/` tree to the Relay's update directory keeps in-app updates working.

## One-time setup

### 1. Android signing secrets (required — the android job fails without them)

The release APK must be signed with the same keystore as previous releases
(`mobile/android-keystore/dsh-release.jks`, not in git). Upload two repo secrets:

```sh
# ANDROID_KEYSTORE_B64: the .jks, base64-encoded
base64 -w0 mobile/android-keystore/dsh-release.jks | gh secret set ANDROID_KEYSTORE_B64

# ANDROID_KEYSTORE_PROPS: the keystore.properties text, verbatim
gh secret set ANDROID_KEYSTORE_PROPS < mobile/android-keystore/keystore.properties
```

(with the GitHub CLI logged in as a repo admin; or paste them in
Settings → Secrets and variables → Actions.)

### 2. Docs submodule — stays private

`docs-repo` points at the **private** `gitee.com/namelessRic/dsh_mobile-docs`.
Every checkout in the workflow uses `submodules: false`, and no build step reads
from it, so the CI never needs a token for it. Do not remove that flag.

### 3. iOS/macOS signing (optional)

The `ios` job runs `tauri ios init` (generates `gen/apple/`) and builds with
`--no-sign`, producing an unsigned IPA for sideload/re-sign. For a
distributable build set `APPLE_DEVELOPMENT_TEAM` + signing certificates and
drop `--no-sign`.

macOS builds ad-hoc by default:

Without Apple signing secrets the `.dmg` is an unsigned/adhoc bundle: it builds
and runs, but Gatekeeper warns on first open. Add `APPLE_CERTIFICATE`,
`APPLE_CERTIFICATE_PASSWORD`, `APPLE_SIGNING_IDENTITY`, `APPLE_ID`,
`APPLE_PASSWORD`, `APPLE_TEAM_ID` if you want a notarized build.

## Releasing

```sh
# bump version in mobile/src-tauri/tauri.conf.json, mobile/package.json, Cargo.toml
git commit -am "release: v0.1.19"
git tag v0.1.19
git push origin main && git push github main
git push github v0.1.19      # <- this triggers the build
```
