/**
 * 通知标识推导（纯函数）。
 *
 * 关键区分（v2）：
 *  - id   = 幂等键：一个逻辑事件一个 id，**不含时间戳**；重复派发/重放同 id。
 *  - slot = 展示槽位：手机通知栏用它做覆盖键；done 卡每会话一个槽。
 * 混用两者会导致「重放被当成新事件」或「同会话多轮互相覆盖」。
 */

export function doneId(sessionId, turn) {
  return `done:${sessionId}:${turn}`
}

export function doneSlot(sessionId) {
  return `done:${sessionId}`
}

export function approvalCardId(sessionId, callId, toolName) {
  return `approval:${sessionId}:${callId ?? toolName ?? 'unknown'}`
}

export function questionCardId(sessionId, callId, fallback) {
  return `question:${sessionId}:${callId ?? fallback ?? 'unknown'}`
}
