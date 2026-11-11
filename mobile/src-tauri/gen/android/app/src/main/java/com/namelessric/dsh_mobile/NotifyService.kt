package com.namelessric.dsh_mobile

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat

/**
 * 通知订阅的前台服务（v1.0.15）。
 *
 * 为什么需要它：SSE 订阅跑在 Rust 的 tokio 任务里，任务跟着 App 进程活。
 * 没有前台服务时，App 切后台一段时间后系统会把进程回收，订阅随之消失，
 * 表现就是「一段时间后收不到审批/提问/完成通知」。
 *
 * 前台服务把进程标成「用户可见的正在进行的工作」，系统不再随意回收；
 * 同时必须常驻一条低优先级通知（Android 的硬性要求），这条通知本身
 * 不打扰用户（IMPORTANCE_MIN + setSilent + 不可滑掉）。
 *
 * 边界（诚实说明）：这是「进程常驻」而不是「开机自启」。用户主动在最近
 * 任务里划掉 App、或 OEM 的极端省电策略仍可能杀掉进程；此时 START_STICKY
 * 只保证服务被重建，不会重建 Tauri 运行时与 SSE 任务——真正的恢复仍靠
 * 用户重新打开 App。v1 不做开机自启（需要持久化凭据 + BootReceiver）。
 */
class NotifyService : Service() {
  companion object {
    private const val TAG = "dsh-notify"
    private const val CHANNEL_ID = "dsh-keepalive"
    private const val NOTIFICATION_ID = 0x445348

    @Volatile
    var running: Boolean = false
      private set

    private const val WAKE_TAG = "dsh:notify-keepalive"
    /** 30 分钟续一次：到期前 onStartCommand 会重新续上。 */
    private const val WAKE_TIMEOUT_MS = 30 * 60 * 1000L

    /** Idempotent: Android collapses repeated startForegroundService calls. */
    fun start(ctx: Context) {
      try {
        val intent = Intent(ctx, NotifyService::class.java)
        ContextCompat.startForegroundService(ctx, intent)
      } catch (e: Throwable) {
        // Android 12+ refuses a background FGS start; the caller runs while
        // the app is foreground, so this is a hard failure worth logging.
        Log.w(TAG, "keepalive start failed: ${e.message}")
      }
    }

    fun stop(ctx: Context) {
      try {
        ctx.stopService(Intent(ctx, NotifyService::class.java))
      } catch (_: Throwable) {
      }
    }
  }

  private var wakeLock: PowerManager.WakeLock? = null

  /** 与 NotifyBridge 共用一个诊断文件，App 下次启动时随日志上传。 */
  private fun logLine(ctx: Context, msg: String) {
    try {
      val f = java.io.File(ctx.cacheDir, "dsh-notify.log")
      if (f.length() > 64 * 1024) f.delete()
      f.appendText("${System.currentTimeMillis()} [service] $msg\n")
    } catch (_: Throwable) {
    }
  }

  /**
   * 持一个 PARTIAL_WAKE_LOCK。
   *
   * 前台服务只保证「进程不被回收」，并不保证 CPU 不休眠：Doze / OEM 省电
   * 策略下网络会被掐断，SSE 就会静默断线（表现：不切前台就收不到通知）。
   * partial wakelock 让 CPU 保持可运行，配合 Rust 侧的 75s 读看门狗，
   * 断线后能在后台自动重连。
   */
  private fun acquireWakeLock() {
    try {
      val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
      val existing = wakeLock
      if (existing != null && existing.isHeld) {
        existing.release()
      }
      wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, WAKE_TAG).apply {
        setReferenceCounted(false)
        acquire(WAKE_TIMEOUT_MS)
      }
    } catch (e: Throwable) {
      logLine(this, "wakelock acquire failed: ${e.message}")
      Log.w(TAG, "wakelock acquire failed: ${e.message}")
    }
  }

  private fun releaseWakeLock() {
    try {
      wakeLock?.let { if (it.isHeld) it.release() }
    } catch (_: Throwable) {
    }
    wakeLock = null
  }

  override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
    val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
    if (Build.VERSION.SDK_INT >= 26) {
      val channel = NotificationChannel(
        CHANNEL_ID,
        "后台通知服务",
        NotificationManager.IMPORTANCE_MIN,
      ).apply {
        description = "保持审批、提问与完成提醒在线"
        setShowBadge(false)
      }
      nm.createNotificationChannel(channel)
    }
    val notification = NotificationCompat.Builder(this, CHANNEL_ID)
      .setSmallIcon(android.R.drawable.stat_notify_sync)
      .setContentTitle("dsh 通知服务")
      .setContentText("保持审批、提问与完成提醒在线")
      .setPriority(NotificationCompat.PRIORITY_MIN)
      .setOngoing(true)
      .setSilent(true)
      .build()
    startForeground(NOTIFICATION_ID, notification)
    acquireWakeLock()
    running = true
    logLine(this, "keepalive running wakeLock=${wakeLock?.isHeld == true}")
    Log.i(TAG, "keepalive running wakeLock=${wakeLock?.isHeld == true}")
    return START_STICKY
  }

  override fun onDestroy() {
    running = false
    releaseWakeLock()
    logLine(this, "keepalive stopped")
    Log.i(TAG, "keepalive stopped")
    super.onDestroy()
  }

  override fun onBind(intent: Intent?): IBinder? = null
}
