package com.namelessric.dsh_mobile

import android.app.Activity
import android.app.AlertDialog
import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * 应用内自动更新桥（Rust 后台任务经 JNI 调用，2026-11-19）。
 *
 * 背景：更新检查移出「进节点」关键路径后，整个「检查 → WiFi 门 → 下载 →
 * 询问 → 安装」都在 Rust/原生侧完成；WebView 已导航到 relay GUI，本桥是
 * 唯一还能和用户交互的入口：
 *  - precheck：WiFi / 以太网判定（计费蜂窝网络不自动下载，用户 2026-11-19 定）；
 *  - showPrompt：下载完成后弹原生对话框问「是否立即安装」，阻塞到用户选择
 *    或超时；选「立即安装」由 Rust 侧 install_apk（InstallBridge）拉系统安装器。
 *
 * 与 NotifyBridge 同一条铁律：这些静态方法只被 Rust 按名字反射调用，Java 侧
 * 没有任何引用，R8 看不见 → proguard-rules.pro 必须 keep（漏 keep =
 * NoSuchMethodError 被 exception_clear 吞掉，后台任务静默失效）。
 */
object UpdateBridge {
  private const val TAG = "dsh-update"
  /** showPrompt 的等待上限：用户一直不回应（App 挂后台）就按「稍后」收场。 */
  private const val PROMPT_TIMEOUT_MINUTES = 5L

  /** 更新链路诊断日志（cacheDir/dsh-update.log，64KB 封顶），随推送日志上传。 */
  fun logLine(ctx: Context, msg: String) {
    try {
      val f = java.io.File(ctx.cacheDir, "dsh-update.log")
      if (f.length() > 64 * 1024) f.delete()
      f.appendText("${System.currentTimeMillis()} $msg\n")
    } catch (_: Throwable) {
    }
  }

  /**
   * 网络预检：返回 JSON `{"wifi":bool}`（call_bridge 只会读回 String）。
   * WiFi / 以太网且已验证连通 → true；蜂窝 / 无网 / 异常 → false（后台任务
   * 不下载，下次启动重查）。VPN 网络会同时声明 TRANSPORT_VPN 与底层传输，
   * WiFi 底座的 VPN 仍按 WiFi 算。
   */
  @JvmStatic
  fun precheck(activity: Activity): String {
    return try {
      val cm = activity.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
      val caps = cm.getNetworkCapabilities(cm.activeNetwork)
      val wifi = caps != null &&
        (caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) ||
          caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) &&
        caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
      logLine(activity, "precheck wifi=$wifi caps=$caps")
      "{\"wifi\":$wifi}"
    } catch (e: Throwable) {
      logLine(activity, "precheck failed: ${e.message}")
      "{\"wifi\":false,\"error\":\"${e.javaClass.simpleName}: ${e.message}\"}"
    }
  }

  /**
   * 下载完成后的安装询问。返回 JSON `{"install":bool}`。
   * 对话框在 UI 线程弹出，调用线程（Rust spawn_blocking）用 latch 阻塞等待；
   * 超时 / 取消 / 用户拒绝都按「稍后」处理，下次启动再问。
   * Activity 正在销毁时不弹（返回 false），避免把对话框挂在尸体 Activity 上。
   */
  @JvmStatic
  fun showPrompt(activity: Activity, message: String): String {
    if (activity.isFinishing || activity.isDestroyed) {
      logLine(activity, "prompt skipped: activity unavailable")
      return "{\"install\":false,\"error\":\"activity unavailable\"}"
    }
    val latch = CountDownLatch(1)
    var install = false
    try {
      activity.runOnUiThread {
        try {
          AlertDialog.Builder(activity)
            .setTitle("发现新版本")
            .setMessage(message)
            .setPositiveButton("立即安装") { _, _ -> install = true; latch.countDown() }
            .setNegativeButton("稍后") { _, _ -> latch.countDown() }
            .setOnCancelListener { latch.countDown() }
            .show()
          logLine(activity, "prompt shown")
        } catch (e: Throwable) {
          // 窗口 token 失效等场景：不能让 latch 永远等下去。
          logLine(activity, "prompt show failed: ${e.message}")
          latch.countDown()
        }
      }
      val answered = latch.await(PROMPT_TIMEOUT_MINUTES, TimeUnit.MINUTES)
      if (!answered) logLine(activity, "prompt timeout ${PROMPT_TIMEOUT_MINUTES}m, treat as later")
      else logLine(activity, "prompt answered install=$install")
    } catch (e: Throwable) {
      logLine(activity, "prompt failed: ${e.message}")
    }
    return "{\"install\":$install}"
  }
}
