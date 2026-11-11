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
- **提问一键作答**（v1.0.15）：单题+单选+≤3 选项时认领，选项做成通知 action；
  多题/多选/选项过多不认领（桌面卡保留），只推提示卡。
- **完成通知**（v1.0.15）：turn 正常结束推 done 卡，带会话标题 + 回复摘要；
  子会话不发，同会话 60s 冷却。
- **隐私**：v1.0.15 起 question 卡带问题文本（用户要求）；approval 卡只带 reason。
- **凭据**：管理 token 只走 header（accessTokenFile）；手机凭据是设备级
  token（扫码配对产物）。推送伪造≠授权：决定仍以手机设备凭据应答。

## 配置（cordis.patch.yml）

```yaml
- insert:
    - id: dsh-approval-push
      name: '@local/dsh-approval-push'
      config:
        relayUrl: 'https://8.148.8.249:24000'
        accessTokenFile: /path/to/relay-access-token
        caFile: /path/to/dsh-relay-ca.crt   # IP 入口必填；域名入口留空
        claimApprovals: true
        askTimeoutMs: 110000
        claimQuestions: true
        questionTimeoutMs: 110000
        notifyDone: true
```

relay 侧需 `phoneDevicesFile` + `notifyEnabled: true`（统一 forwarder 的
server.json `relay` 段）。

## 手机端

App 1.0.15+ 在前台/后台存活时维持 SSE 订阅（`notify/watch`），收到
approval 卡弹原生通知：允许一次 / 拒绝 两个 action + 点击进 App；
question 卡带问题文本与选项按钮（可直接作答）；done 卡带会话标题与摘要。
1.0.15 起用前台服务（`NotifyService`，specialUse）保持订阅常驻；
主动划掉 App 或 OEM 极端省电仍会中断（v1 不做开机自启）。