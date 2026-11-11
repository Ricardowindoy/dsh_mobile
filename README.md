# dsh-mobile — dsh 手机访问

在手机上随时访问自建 [DeepSeek Harness（dsh）](https://gitee.com/namelessRic/dsh_mobile) 的 Web GUI。dsh 机器位于家庭/办公 NAT 之后（无公网 IP），通过一台公网 **Relay** 服务器中转：手机 App 与 dsh 机器建立连接，dsh Web GUI 原样跑在手机上。

- **移动端**：[Tauri v2](https://tauri.app/) 原生 App（Android/iOS），内嵌 WebView 加载 dsh Web GUI（已移动适配），并通过 Relay 与 dsh 机器通信。
- **服务端**：Golang 实现 —— `dsh-relay`（公网中转，纯字节管道）+ `dsh-tunnel`（dsh 机器侧出站连接器，穿透 NAT）。

> 背景与设计：见私有子模块 [`docs-repo/`](./docs-repo)（`dsh_mobile-docs`，不随主仓公开）。

## 架构

```
手机（Tauri App，WebView 内为 dsh Web GUI）
   │ HTTPS + 用户鉴权（TLS 在 Relay 入口终结）
   ▼
公网 Relay 服务（dsh-relay，VPS）
   │ 配对后路由：phone → relay → device 的字节管道
   │ 设备主动出站 wss（穿透 NAT，无需公网 IP/端口映射）
   ▼
dsh 机器：dsh-tunnel → 127.0.0.1:3080（dsh web 原样，零配置改动）
```

**原则：Relay 是纯字节管道**，不解析 dsh 业务协议；鉴权边界在 Relay，隧道内视为可信回环。

## 仓库结构

```
├── cmd/
│   ├── relay/        dsh-relay 公网中转服务（Go）
│   └── tunnel/       dsh-tunnel 出站连接器（Go）
├── internal/
│   ├── protocol/     共享 wire 协议：类型、codec、帧上限、心跳常量
│   ├── mux/          wss 多路复用：背压、流控、半关闭、心跳
│   ├── relay/        Relay 核心：配对、路由、鉴权门、限额
│   └── tunnel/       出站连接器：重连、头归一化、转发
├── e2e/              三件套集成测试（真实 HTTP/WS 经 relay+tunnel 往返）
├── mobile/           Tauri v2 移动端 App
├── docs-repo/        协议说明、部署文档（私有子模块，不随主仓公开）
├── deploy/           docker / systemd 部署示例
└── LICENSE           GPLv3
```

## 隧道协议（v1）

- **设备 ↔ Relay**：单条 wss 连接，多路复用：
  - 控制帧（文本 JSON）：`hello{v,deviceId,token}` → `ok{heartbeatMs}` / `err{reason}`；`stream.open{sid,method,path,headers,upgrade}` → `stream.accept{sid,status,headers}` / `stream.reject{sid,status}`；`stream.end{sid}`（半关闭）；`ping`/`pong` 心跳。
  - 数据帧（二进制）：`[4B streamId][payload]`，帧上限 64KB。
- **手机 ↔ Relay**：普通 HTTP/1.1；`upgrade: websocket` 的流按原始字节管道透传（dsh 的 `/api/events.*` 走同一条流）。
- **流控**：每流缓冲上限 + 背压（上游阻塞不丢帧）；最大并发流数；idle 超时；设备重连指数退避 1s→30s（含 jitter）；可选每设备带宽限额。
- 详见 [docs/protocol.md](./docs/protocol.md)。

## 快速开始

### 1. Relay（公网 VPS）

```bash
# 配置配对：token → deviceId（一条 token 对应一台 dsh 机器）
cat > pairings.json <<'EOF'
{"change-me-token": "home-dsh"}
EOF

go build -o dsh-relay ./cmd/relay
./dsh-relay \
  --listen :8443 \
  --tls-cert /etc/letsencrypt/live/relay.example.com/fullchain.pem \
  --tls-key  /etc/letsencrypt/live/relay.example.com/privkey.pem \
  --pairings pairings.json \
  --access-token "$(openssl rand -hex 32)"
```

> 无证书时可先 `--listen :8080`（HTTP）本地验证；生产必须 TLS + `--access-token`，或前置反代做 Basic Auth（见 `docs-repo/docs/deployment.md`，私有子模块）。

### 2. Tunnel（dsh 机器）

```bash
go build -o dsh-tunnel ./cmd/tunnel
DSH_TUNNEL_PAIRING_TOKEN=change-me-token ./dsh-tunnel \
  --relay wss://relay.example.com:8443/__dsh_tunnel__/device \
  --device-id home-dsh \
  --local 127.0.0.1:3080
```

配对 token 只经环境变量注入（credentials seam），不进代码、不进命令行。

### 3. 手机 App（Tauri）

见 [mobile/README.md](./mobile/README.md)。构建 Android APK 后安装，在 App 内填入 Relay 地址（如 `https://relay.example.com`）与访问 token，即可全屏使用 dsh Web GUI。

App 更新检查与下载**优先走 GitHub Releases**（公开仓库的
`releases/latest/download/manifest.json` 与对应安装包），GitHub 不可达或下载失败时
自动回退到 Relay 的更新站点（`/__dsh_tunnel__/update/check` / `download`）。

## CI 打包

GitHub Actions：[`.github/workflows/release-build.yml`](./.github/workflows/release-build.yml)

| 平台 | 产物 | Runner |
| --- | --- | --- |
| Android | 签名 release APK（arm64）+ AAB | ubuntu-22.04 |
| Windows | NSIS 安装包 `*-setup.exe` | windows-latest |
| Linux | `.deb` / `.rpm` / `.AppImage` | ubuntu-22.04 |
| macOS | universal `.dmg`（arm64 + x86_64） | macos-latest |
| iOS | `dsh-mobile-unsigned.ipa`（arm64，未签名） | macos-latest |
| Tunnel | `cmd/tunnel` linux{amd64,arm64} / darwin-arm64 / windows-amd64 | ubuntu-latest |

推 `v*` tag 或手动触发；汇总 job 生成 `release/manifest.json`（与 `scripts/release.sh` 同构）并发布 GitHub Release。
首次使用需配置 Android 签名 secrets，见 [`.github/RELEASE-SECRETS.md`](./.github/RELEASE-SECRETS.md)。

## iOS 安装（未签名 IPA）

iOS 真机不允许安装纯自签证书的包；Apple 只认它签发的证书 + provisioning profile。
Release 里的 `dsh-mobile-unsigned.ipa` 是**未签名包**，需要借助下面三种方式之一，用你自己的
Apple ID 在本机签名后安装。三种方式都无需 Apple 开发者付费账号（TrollStore 甚至不需要 Apple ID）。

### 方式 A：AltStore / SideStore（推荐，免费 Apple ID，7 天自动续签）

1. 电脑（Windows 或 macOS）安装 **AltServer**（<https://altstore.io>）或 **SideStore**（<https://sidestore.io>）。
2. 手机用 USB 连接电脑，通过 AltServer 把 AltStore 装到手机；首次需要登录你的 Apple ID。
3. 手机上打开「设置 → 通用 → VPN 与设备管理」，信任对应的开发者证书。
4. 下载 Release 里的 `dsh-mobile-unsigned.ipa`，在 AltStore 的「My Apps → +」中选择该 IPA，用 Apple ID 签名安装。
5. 免费签名 **7 天过期**。保持手机与运行 AltServer 的电脑在同一 Wi-Fi，AltStore 会在后台自动续签；否则需重新签名。

### 方式 B：Sideloadly（Windows / macOS，拖拽即签）

1. 下载 **Sideloadly**：<https://sideloadly.io>。
2. 手机连接电脑，把 `dsh-mobile-unsigned.ipa` 拖入 Sideloadly，填写 Apple ID，点 Start。
3. 首次安装后在「设置 → 通用 → VPN 与设备管理」信任证书。
4. 同样是 7 天免费签名，到期后重新拖入 IPA 签一次即可。

### 方式 C：TrollStore（仅限受支持的系统版本，永久免签）

1. 先确认你的 iOS 版本在 TrollStore 支持范围内（见 <https://github.com/opa334/TrollStore> 的兼容表）。
2. 按 TrollStore 官方指引完成安装。
3. 把 `dsh-mobile-unsigned.ipa` 分享/导入 TrollStore 安装，签名永久有效，无需续签。

### 免费 Apple ID 的限制

- 签名 **7 天过期**，每台设备最多 **3 个自签 App**，每 7 天最多 **10 个 App ID**。
- 免费 provisioning 不支持 Push Notifications、Associated Domains 等能力（本项目审批推送走本地通知，不受影响）。
- **不要把 Apple ID 交给任何第三方“免费签名平台”**；AltServer / SideStore / Sideloadly 都是在本机完成签名的开源/可信工具。

## 安全模型

- **RCE-as-a-service**：对通过鉴权的人而言，dsh 就是宿主机 shell。Relay 必须 TLS + 强鉴权；部署文档建议为 dsh 启用 Landlock 沙箱限制 agent 执行范围（见 `docs-repo/docs/deployment.md`，私有子模块）。
- **头归一化**：tunnel 把转发请求的 `Host` 改写为回环、剥离 `Origin/Sec-Fetch-Site`，让流量以"本地用户"身份过 dsh 现有信任栅栏——鉴权已上移到 Relay（同 SSH 端口转发的信任模型）。
- **配对失败拒绝**：错误 token 被拒绝并记录；同一 deviceId 重复连接会踢掉旧连接。

## 构建与发布约定

- **改前端必须重编 Rust**：`tauri-codegen` 把 `dist/` 编进 `.so`，只跑 gradle 交付的是旧界面；同时三处版本号同步升级（`tauri.conf.json` / `Cargo.toml` / `package.json`），`CARGO_PKG_VERSION` 是 WebView 缓存自愈的判据。
- **gradle 禁 daemon**（已写死 `gen/android/gradle.properties`）：daemon 会跨调用存活并在旧命名空间里让 tauri CLI 的 RPC 握手 `ConnectionRefused`；遇「间歇 ConnectionRefused」先 `pkill -f GradleDaemon`。
- **release profile 必须 `lto="thin" + opt-level="z"`**：fat LTO 在内存有限的构建机上会 OOM（exit 134）。
- **连接页样式硬规矩**：样式表常驻 `[hidden] { display: none !important; }`（flex 容器会盖掉 UA 默认的 hidden 语义）；居中用 `margin:auto 0`，不要 `justify-content:center`（软键盘压矮视口时会把内容推出可滚动区）。
- **发布流程**：bump 版本三处 → `scripts/release.sh`（relay 更新站）或 `mobile/scripts/release-github.sh`（GitHub Release，第一优先级）→ `curl update/check` 校验 sha256 与下载 URL。

## 开发

```bash
go test ./...        # 全部单测 + e2e（真实 HTTP/WS 三件套往返）
go vet ./...
```

## 许可

[GPLv3](./LICENSE)
