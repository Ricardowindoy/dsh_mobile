# @local/dsh-remote-access

DSH 远程访问的**管理面**插件：在设置页切换「被控节点」与「Relay 入口」，查看 relay 探测、本机服务状态，并提供 Agent 工具。

数据面是另一个进程（`dsh-forwarder` + `dsh-tunnel`），插件不托管它。

> **多节点切换（2026-11-10 起，只留一套）**：手机 App 经 relay 打开时，relay
> 根路径返回一个**多节点壳页**（同源 iframe 每节点常驻，切换 0 重载），壳页
> 本身不渲染任何下拉。节点切换 UI 由本插件 `NodeSwitch` 下拉提供（会话头部
> 标签行右端）。**唯一链路**：所有入口（下拉/设置页/Agent 工具）汇到
> `switchNode()` 直发 relay `/__dsh_tunnel__/default-node`；壳页订阅
> `default-node/watch`(SSE) 跟随切 iframe；桥 `state/subscribe` 只做读。
> 桌面直连不经 relay 时由 host `POST /api/remote-access/config` 代发到同一个
> default-node 写入（服务端 `applySwitch` 唯一函数），无整页 reload。
> 详见 `docs/multi-node-shell-design.md`。

## 功能

| 功能 | 说明 |
|---|---|
| 被控节点切换 | 节点 = 一台被控机器；同 relay 下用 `/d/<deviceId>/` 路由。切换只改变观测与入口 URL，不重启服务；relay 壳页下为 iframe 瞬时切换（0 重载），无壳页时整页 reload 兜底 |
| Relay 入口切换 | 同一 relay 的不同访问路径（域名 / IP 直连） |
| 状态观测 | relay probe（可达 / 鉴权 / 设备在线 / 每设备在线列表）、本机服务状态、隧道 lifecycle 日志尾、frpc 代理表 |
| Agent 工具 | `remote_access_status`、`remote_access_nodes`、`remote_access_switch`、`remote_access_restart`、`/remote-status` |
| Windows / Linux | Linux 用 systemd（user/system）；Windows 用 Windows 服务（`sc.exe`） |

## 配置（`cordis.patch.yml`）

```yaml
- insert:
    - id: dsh-remote-access
      name: '@local/dsh-remote-access'
      config:
        activeEndpoint: ip
        activeNode: home-dsh
        allowRestart: false
        endpoints:                      # relay 入口
          - id: ip
            label: IP 直连
            probeUrl: 'https://relay.example.com:24000/__dsh_tunnel__/probe'
            caFile: /path/to/dsh-relay-ca.crt        # 私有 CA 时必填
            accessTokenFile: /path/to/relay-access-token  # 有它才能拿到每设备在线列表
        nodes:                          # 被控节点
          - id: home-dsh
            label: 本机
            deviceId: home-dsh          # 必须与那台机器 dsh-tunnel 的 --device-id 一致
            relayUrl: 'https://relay.example.com:24000'
            caFile: /path/to/dsh-relay-ca.crt
            accessTokenFile: /path/to/relay-access-token
        # services: 可选，显式覆盖下面的平台默认值
```

### 平台默认值

`services` 为空时：

- **Linux**：从 endpoint 的 `tunnelUnit` / `frpcUnit` / `frpcGatewayUnit` 派生 `systemd-user` 单元；都没有则用 `dsh-tunnel.service`、`frpc.service`、`frpc-gateway-8326.service`。
- **Windows**：`dsh-tunnel`、`dsh-forwarder-client`（`windows-service`）。

Windows 显式配置示例：

```yaml
        services:
          - id: tunnel
            label: dsh-tunnel
            kind: windows-service
            name: dsh-tunnel
          - id: forwarder
            label: dsh-forwarder-client
            kind: windows-service
            name: dsh-forwarder-client
```

`kind` 取值：`systemd-user` | `systemd-system` | `windows-service`。

## 新增一台被控节点

1. 在那台机器上运行 `dsh-tunnel`（Windows 用 `install-windows.ps1` 装成服务），使用**不同的** `--device-id` 和 pairing token，连到同一个 relay：

   ```bash
   dsh-tunnel --relay wss://<relay>/__dsh_tunnel__/device \
     --device-id office-pc --local 127.0.0.1:3082
   ```

2. 在 relay 的 `pairings.json` 里加一条 `"<token>": "office-pc"`。
3. 在插件 `nodes` 里加一项 `{id: office-pc, deviceId: office-pc, relayUrl: ...}`。
4. 设置页「被控节点」里点「切换到此节点」，或让 Agent 调 `remote_access_switch({nodeId:"office-pc"})`。
5. 节点地址为 `https://<relay>/d/<deviceId>/`（调试/单节点直连用；手机 App
   填 **relay 根地址**即可，壳页会自动列出所有已配对节点供切换，见
   `docs/multi-node-shell-design.md`）。


## dsh-tunnel 数据面托管安装（v1.3.0 新增）

设置页新增「数据面安装（dsh-tunnel）」分区；Agent 侧新增三个工具。**架构不变**：隧道仍是
独立进程，插件只做下载/安装/卸载托管，绝不把隧道逻辑跑进 dsh-web 进程。

| 工具 | 说明 |
|---|---|
| `remote_access_tunnel_install_status` | 本机安装状态（二进制路径/大小、服务注册状态、unit 是否本插件托管）。只读 |
| `remote_access_tunnel_install` | 下载（relay update 通道，SHA-256 校验）→ 原子写入 `<DSH_HOME>/bin/` → 注册 systemd 用户单元（Linux）或生成 sc.exe 安装脚本（Windows） |
| `remote_access_tunnel_uninstall` | 停止并删除本插件生成的单元（手动写的 unit 一律拒绝覆盖） |

启用前置（总闸默认关）：`cordis.patch.yml` 里 `tunnelInstall.allowInstall: true`，可选
`deviceId` / `relayBase` / `localAddr` / `pairingTokenFile` / `caFile` / `accessTokenFile` / `extraArgs`。
未开启时所有安装操作返回明确的 disabled 说明，不做任何写动作。

Relay 侧需要 update 通道里有 `platform=tunnel-<os>` 条目（`updates/manifest.json` +
`updates/tunnel-<os>/<arch>/dsh-tunnel-*`）；`scripts/release.sh` 生成的 Android 清单不受影响。

诚实性约束：安装状态读不到就是 `absent`/`unknown`，不兜底；SHA-256 或 size 不匹配立即失败并
清掉暂存文件；unit 已存在且非本插件写入时拒绝覆盖（变异检验覆盖）。

## 安全

- `probe` 的聚合结果（`deviceOnline/deviceCount`）公开；**每设备列表只在带 access token 时返回**，避免公开设备 id。
- 插件不保存 token：`accessTokenFile` 指向本机 0600 文件。
- `allowRestart` 默认 `false`；开启后也只允许重启配置里列出的服务。

## 会话头部节点下拉（`NodeSwitch`）

- 座位 `shell.overlay`（常驻，空白会话也在）；实际按钮 portal 到会话头部标签行
  右端（`[data-conversation-tabs]` 的父容器），无标签行时固定右上角兜底。
- 菜单样式（2026-11-10 用户定）：一行 = 状态色点（绿在线 / 红离线 / 灰未观测）
  + `deviceId` + 行尾选中勾；卡片宽度随内容收缩（`width:max-content`，
  390px 视口实测约 190px），无标题/副行/胶囊。色点 `aria-hidden`，按钮
  `aria-label` 带语义词，不靠颜色猜。
- 切换（只留一套）：下拉与设置页都调 `switchNode(deviceId)` 直发 relay
  `/__dsh_tunnel__/default-node`；壳页 SSE 跟随切 iframe、再 `subscribe` 回来
  更新选中态。桥 `switchTo/takeover/release/beat` 均为兼容空实现（不承载写入）。
- 桥读：`parent.__dshShell.state()/subscribe()` 读在线态与当前节点，缺失不猜。
- 桌面直连（不经 relay）：`switchNode` 的 404 回退走 host `POST config` 代发，
  host 内 `applySwitch` 复用同一个 `setRelayDefaultNode` 写入，无整页 reload、
  无插件侧 node-watch。

## 测试

```bash
node --test test/plugin.test.mjs
```

覆盖：节点 URL 编码、Windows 服务状态解析（locale 无关的数字 STATE）、probe 设备列表解析、平台服务派生（Linux/Windows）、状态持久化与非法 id 拒绝、多节点聚合回退、restart 闸门。

## 移动端 UI 适配 skill（不再随插件分发）

`dsh-mobile-ui-adaptation`（DSH Web 移动端 UI 适配流程：构建链顺序、隔离工作树、几何验证、生产替换）现由**本机技能库** `~/.agents/skills/dsh-mobile-ui-adaptation/SKILL.md` 提供。

- 2026-11-10 起插件不再内嵌 `skills/` 目录，也不再注册 bundled skill provider；管理面功能与此 skill 完全解耦。
- 需要在其它部署（如 Windows 节点）使用该 skill 时，把 `SKILL.md` 复制到该机器的用户/项目级 skills 目录即可。
