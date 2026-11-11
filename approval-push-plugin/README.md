# 审批推送（approval-push）

把 DSH 的审批请求/提问推送到已配对手机的通知卡片。语义借自上游
`@linxin666/dsh-remote-bridge`（claim / fail-closed / 重连重放），落在我们
自己的 relay 通知面（`/__dsh_tunnel__/notify/*`）上。

## 硬规则

- **有订阅才认领**：`approval/request` waterfall answerer 先问 relay
  `notify/online`；没有手机在线就 `next()`，桌面审批行为一字不变。
- **沉默不是同意**：认领后超时（默认 110s）无人应答 → 返回 undefined →
  approval 服务归一为 `unavailable`（拒绝）。
- **先答先赢**：GUI 先答 → `approval/decided` 审计事件到 → 采用其 outcome
  并向手机发 retract 卡。
- **隐私**：question 卡不带问题文本。
- **凭据**：管理 token 只走 header（accessTokenFile）；手机凭据是设备级
  token（扫码配对产物）。推送伪造≠授权：决定仍以手机设备凭据应答。

## 配置（cordis.patch.yml）

```yaml
- insert:
    - id: dsh-approval-push
      name: '@local/dsh-approval-push'
      config:
        relayUrl: 'https://relay.example.com:24000'
        accessTokenFile: /path/to/relay-access-token
        caFile: /path/to/dsh-relay-ca.crt   # IP 入口必填；域名入口留空
        claimApprovals: true
        askTimeoutMs: 110000
```

relay 侧需 `phoneDevicesFile` + `notifyEnabled: true`（统一 forwarder 的
server.json `relay` 段）。

## 手机端

App 0.1.19+ 在前台/后台存活时维持 SSE 订阅（`notify/watch`），收到
approval 卡弹原生通知：允许一次 / 拒绝 两个 action + 点击进 App。
进程被杀后无通知（v1 已知限制，无常驻服务）。