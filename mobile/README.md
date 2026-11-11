# dsh-mobile（移动端 App）

Tauri v2 原生壳：内嵌 WebView 加载经公网 Relay 中转的 dsh Web GUI（dsh 已完成移动端适配），提供：

- 首次启动的设置页：Relay 地址 + 访问 token（存 localStorage）；
- 自动经 `/__dsh_tunnel__/login` 植入鉴权 cookie 后跳入 dsh GUI；
- 之后启动直接进入 dsh；重新配置入口：先清数据或重新安装。

> 说明：App 自身只做"壳"，不解析 dsh 业务协议——dsh Web GUI 原样运行，认证与转发都在 Relay 侧完成（见仓库根 [README](../README.md) 与 [docs/protocol.md](../docs/protocol.md)）。

## 构建（Android）

前置：Rust（含 `aarch64-linux-android` 等 Android target）、Android SDK + NDK、JDK 17、pnpm。

```bash
export ANDROID_HOME=/path/to/Android/Sdk
export JAVA_HOME=/path/to/jdk17   # 需完整 JDK（含 javac，JRE 不行）

pnpm install --store-dir ./.pnpm-store   # 本机 pnpm store 盘只读时
pnpm tauri android build --debug         # 调试包（大，含调试符号）
pnpm tauri android build --target aarch64   # release 包（自签证书签名，走 gen/android 里的 signingConfig）
```

**Release 签名**：`gen/android/app/build.gradle.kts` 从 `mobile/android-keystore/keystore.properties` 加载自签 keystore（本仓库已生成 `dsh-release.jks`，该目录已 gitignore，勿提交密钥）。首次构建 release 前若目录不存在：

```bash
mkdir -p android-keystore
keytool -genkeypair -v -keystore android-keystore/dsh-release.jks \
  -alias dsh -keyalg RSA -keysize 2048 -validity 10000 \
  -storepass <密码> -keypass <密码> \
  -dname "CN=dsh-mobile, OU=dsh, O=namelessRic, L=Home, ST=China, C=CN"
# 再写 android-keystore/keystore.properties（storeFile/storePassword/keyAlias/keyPassword）
```

产出：`gen/android/app/build/outputs/apk/arm64/release/app-arm64-release.apk`。

## 应用内版本更新

- **优先 GitHub Release**：`api.github.com/repos/Ricardowindoy/dsh_mobile/releases/latest`，资产名 `app-arm64-release.apk`（同名 `.sha256` 可选）。
- GitHub 不可达/无可用资产时**回退 Relay**：`/__dsh_tunnel__/update/check` + `/download`，读 `updates/manifest.json`。
- 发布：`../scripts/release.sh` 生成 relay manifest；GitHub Release 由 CI 自动发布（tag `v*`）。

## iOS 安装（未签名 IPA）

CI 产出 `dsh-mobile-unsigned.ipa`（未签名）。安装方式：AltStore/SideStore（免费 Apple ID，7 天自动续签）、Sideloadly（拖拽签名）、TrollStore（受支持系统版本，永久免签）。详见仓库根 [README 的 iOS 安装章节](../README.md#ios-安装未签名-ipa)。

## 结构

```
src/            前端壳页面（设置页 + 更新横幅 + 跳转逻辑，无框架）
src-tauri/      Rust 壳 + Tauri 配置（capabilities、图标、opener 插件）
android-keystore/   release 自签 keystore（已 gitignore）
gen/android/    Tauri 生成的 Android Studio 工程（release 签名配置在
                gen/android/app/build.gradle.kts，已入库）
```

## 许可

GPLv3（随仓库根 [LICENSE](../LICENSE)）。
