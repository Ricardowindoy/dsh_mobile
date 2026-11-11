#!/usr/bin/env python3
"""Build release/manifest.json by scanning the release/ tree.

Layout produced by capture-release.sh / the tunnel job:

    release/<platform>/<arch>/<file>
    e.g. release/android/arm64/app-universal-release.apk
         release/linux/amd64/dsh-mobile_0.1.18_amd64.deb
         release/tunnel-windows/amd64/dsh-tunnel-windows-amd64.exe

The manifest is consumed by the Relay's /__dsh_tunnel__/update/check and
/download endpoints and by the App's in-app updater. It has the same shape
`scripts/release.sh` writes locally, so the two paths stay interchangeable.

One item per (platform, arch) is emitted, choosing the canonical file for
that platform (APK over AAB, DEB over RPM/AppImage, etc.). Sizes and
SHA-256 are computed from the files on disk — never trusted from a
hand-written TSV.

Usage: build-manifest.py [--note TEXT] [--force]
"""
import hashlib
import json
import os
import sys
from datetime import date

ROOT = os.environ.get("GITHUB_WORKSPACE", os.getcwd())
REL = os.path.join(ROOT, "release")
OUT = os.path.join(REL, "manifest.json")

# Canonical artifact per platform: (extension, suffix) in priority order.
CANONICAL = {
    "android": [(".apk", "")],
    "windows": [(".exe", "")],
    "linux": [(".deb", ""), (".AppImage", ""), (".rpm", "")],
    "macos": [(".dmg", ""), (".app", "")],
    "ios": [(".ipa", "")],
}
DEFAULT_CANONICAL = [("", "")]  # tunnel-*: exact binary name
NOTE_TAILS = {
    "android": "Android 客户端（arm64，自签）",
    "windows": "Windows 客户端",
    "linux": "Linux 客户端",
    "macos": "macOS 客户端",
    "ios": "iOS 客户端（未签名）",
    "tunnel-linux": "Tunnel 数据面",
    "tunnel-darwin": "Tunnel 数据面",
    "tunnel-windows": "Tunnel 数据面",
}


def sha256(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def pick(platform, files):
    rules = CANONICAL.get(platform, DEFAULT_CANONICAL)
    for ext, _ in rules:
        hits = sorted(f for f in files if f.endswith(ext))
        if hits:
            # Ignore helper files such as .aab for the manifest item.
            return hits[0]
    return None


def main() -> int:
    note = date.today().isoformat()
    forced = "--force" in sys.argv
    if "--note" in sys.argv:
        note = sys.argv[sys.argv.index("--note") + 1]

    if not os.path.isdir(REL):
        print("::error::release/ does not exist — nothing was captured", file=sys.stderr)
        return 1

    version = "0.0.0"
    try:
        with open(os.path.join(ROOT, "mobile", "src-tauri", "tauri.conf.json"), encoding="utf-8") as fh:
            version = json.load(fh)["version"]
    except Exception as exc:  # noqa: BLE001
        print(f"::warning::could not read tauri.conf.json version: {exc}", file=sys.stderr)

    items, skipped = [], []
    for platform in sorted(os.listdir(REL)):
        pdir = os.path.join(REL, platform)
        if not os.path.isdir(pdir) or platform.startswith("."):
            continue
        for arch in sorted(os.listdir(pdir)):
            adir = os.path.join(pdir, arch)
            if not os.path.isdir(adir):
                continue
            # Auxiliary bundle dirs (arm64-aab, amd64-rpm, amd64-appimage)
            # are release assets, not updater manifest entries.
            if "-" in arch:
                continue
            files = [f for f in sorted(os.listdir(adir)) if os.path.isfile(os.path.join(adir, f))]
            if not files:
                skipped.append(f"{platform}/{arch} (empty)")
                continue
            chosen = pick(platform, files)
            if chosen is None:
                skipped.append(f"{platform}/{arch}: no canonical file among {files}")
                continue
            path = os.path.join(adir, chosen)
            item = {
                "platform": platform,
                "arch": arch,
                "version": version,
                "filename": chosen,
                "size": os.path.getsize(path),
                "sha256": sha256(path),
                "releaseNote": f"{note} {NOTE_TAILS.get(platform, '')}".strip(),
                "releaseAt": date.today().isoformat(),
                "force": forced,
            }
            items.append(item)
            # App's GitHub update path downloads <asset>.sha256 and verifies it.
            with open(path + ".sha256", "w", encoding="utf-8") as sh:
                sh.write(f"{item['sha256']}  {chosen}\n")
            print(f"  + {platform}/{arch}: {chosen} ({item['size']} bytes)")

    if not items:
        print("::error::release/ contains no usable artifacts — refusing to write an empty manifest",
              file=sys.stderr)
        return 1
    if skipped:
        for s in skipped:
            print(f"::warning::skipped {s}", file=sys.stderr)

    with open(OUT, "w", encoding="utf-8") as fh:
        json.dump({"items": items}, fh, ensure_ascii=False, indent=4)
        fh.write("\n")

    print(f"wrote {OUT} with {len(items)} item(s) at version {version}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
