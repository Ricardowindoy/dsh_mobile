package com.namelessric.dsh_mobile

import android.app.Activity
import android.content.res.Configuration
import android.os.Build
import android.os.Bundle
import android.util.Log
import android.view.WindowInsetsController
import android.webkit.WebView
import java.lang.Thread
import androidx.core.content.ContextCompat
import androidx.core.view.ViewCompat
import androidx.core.view.WindowCompat
import androidx.core.view.WindowInsetsCompat
import kotlin.math.pow

class MainActivity : TauriActivity() {
  companion object {
    init {
      // Rust 侧需要经此拿到 JavaVM/Activity（自动更新安装 + 状态栏主题切换）；
      // 库本身由 generated Rust object 已加载，重复 load 幂等。
      System.loadLibrary("dsh_mobile_lib")
    }

    @JvmStatic
    external fun nativeInitBridge(activity: Activity)
  }

  /**
   * 条带色是否已由前端接管（进 GUI 后 set_status_theme 会指定页面自己的底色）。
   * true 之后系统切明暗不再回刷 canvas 色 —— 否则会把 GUI 的条带改成连接页底色，
   * 出现"页面是 GUI、条带是连接页"的错位。App 重启回连接页时自然复位。
   */
  private var stripOwnedByPage = false

  override fun onCreate(savedInstanceState: Bundle?) {
    super.onCreate(savedInstanceState)
    logLifecycle("onCreate")
    // Android 13+ 通知权限：在启动可见阶段请求。放在 connect() 里请求会被
    // 紧接着的页面导航打断，用户来不及点，通知就一直发不出来。
    if (Build.VERSION.SDK_INT >= 33 &&
      ContextCompat.checkSelfPermission(this, android.Manifest.permission.POST_NOTIFICATIONS) !=
      android.content.pm.PackageManager.PERMISSION_GRANTED
    ) {
      requestPermissions(arrayOf(android.Manifest.permission.POST_NOTIFICATIONS), 1002)
    }
    // ---- 诊断（白屏/ANR 排查，v0.1.17 后可按需移除）----
    // 打开 WebView 远程调试：release 包默认关闭，开启后可用 chrome://inspect
    // （或 adb forward 到 webview_devtools_remote_<pid> + CDP）直接查看页面 URL、
    // DOM 与控制台报错。这是唯一能看到 WebView 内部状态的通道。
    runCatching { WebView.setWebContentsDebuggingEnabled(true) }
      .onFailure { Log.w("dsh-diag", "setWebContentsDebuggingEnabled failed: $it") }

    // 主线程看门狗：ANR 的系统 trace 在 release 包上读不到（/data/anr 权限），
    // 所以自己在主线程卡住时把全线程栈打到 logcat。判定用「主线程 Handler 能否
    // 在 deadline 内执行一个空任务」——卡住就说明主线程被占了。
    startMainThreadWatchdog()

    val t0 = System.currentTimeMillis()
    Log.i("dsh-diag", "onCreate START")
    // 把 Activity 递给 Rust（JNI 桥入口：自动更新 + 状态栏主题切换）
    nativeInitBridge(this)
    Log.i("dsh-diag", "nativeInitBridge done +${System.currentTimeMillis() - t0}ms")
    // 初始主题 = 连接页（跟随系统明暗）：targetSdk 36 强制 edge-to-edge，
    // 状态栏条带是 decorView 的 padding 区，颜色随 decorView 背景走。
    // 底色取 @color/canvas（values / values-night 各一份），必须与
    // mobile/src/styles.css 的 --set-bg 同值，否则条带与页面之间有色差；
    // 不再写死 #0f1115 字面量，否则切深色模式时条带会留在旧底色。
    applyCanvasTheme()
    // 软键盘顶起：adjustResize 在部分 ROM/WebView 组合下不生效（窗口不
    // 随 IME 收缩）。这里手动监听 insets：顶部补状态栏高度、左右补刘海
    // 安全区（修「顶部 UI 与通知栏重叠」），底部把 WebView 容器 padding
    // 设为键盘高度，等效把内容顶起来。键盘收起时底部 padding 归零。
    ViewCompat.setOnApplyWindowInsetsListener(window.decorView) { view, insets ->
      val ime = insets.getInsets(WindowInsetsCompat.Type.ime())
      val systemBars = insets.getInsets(WindowInsetsCompat.Type.systemBars())
      val bottomPad = if (ime.bottom > systemBars.bottom) ime.bottom - systemBars.bottom else 0
      view.setPadding(systemBars.left, systemBars.top, systemBars.right, bottomPad)
      Log.d(
        "dsh-insets",
        "top=${systemBars.top} left=${systemBars.left} right=${systemBars.right} " +
          "ime.bottom=${ime.bottom} systemBars.bottom=${systemBars.bottom} padBottom=$bottomPad",
      )
      insets
    }
    Log.i("dsh-diag", "onCreate END +${System.currentTimeMillis() - t0}ms")
  }

  // ---- 诊断：主线程看门狗 ----
  // 每 1s 检查主线程是否还能在 2s 内跑一个空任务；连续 3 次不能判定卡死，
  // 立刻 dump 所有线程栈（含 native 帧）到 logcat，然后停止，避免刷屏。
  private fun startMainThreadWatchdog() {
    val main = android.os.Handler(android.os.Looper.getMainLooper())
    var misses = 0
    val tick = object : Runnable {
      override fun run() {
        var responded = false
        // post 一个任务到主线程；本 run() 已在后台线程（见下方 Thread）。
        main.post { responded = true }
        Thread.sleep(2000)
        if (responded) {
          misses = 0
        } else if (++misses >= 3) {
          Log.e("dsh-diag", "MAIN THREAD STUCK ${misses * 2}s — dumping all stacks")
          dumpStacks("watchdog")
          return
        }
      }
    }
    Thread {
      while (true) {
        try { tick.run() } catch (e: Throwable) {
          Log.w("dsh-diag", "watchdog error: $e")
        }
      }
    }.apply { name = "dsh-watchdog"; isDaemon = true }.start()
  }

  /** 把当前进程所有线程的栈打到 logcat（tag=dsh-stack）。 */
  private fun dumpStacks(reason: String) {
    val pid = android.os.Process.myPid()
    Log.e("dsh-stack", "==== stack dump ($reason) pid=$pid ====")
    try {
      val map = Thread.getAllStackTraces()
      for ((thread, stack) in map) {
        Log.e("dsh-stack", "--- ${thread.name} state=${thread.state} ---")
        for (el in stack) Log.e("dsh-stack", "    at $el")
      }
    } catch (e: Throwable) {
      Log.e("dsh-stack", "dump failed: $e")
    }
    Log.e("dsh-stack", "==== end stack dump ====")
  }

  /**
   * 生命周期落盘：用户报"打开就切后台"时，logcat 拿不到，只能靠这份文件
   * 还原"什么时候被切走"。Rust 的 read_crash_log 会在下次启动读走并上传。
   */
  private fun logLifecycle(event: String) {
    try {
      val f = java.io.File(cacheDir, "dsh-lifecycle.log")
      if (f.length() > 64 * 1024) f.delete()
      f.appendText("${System.currentTimeMillis()} $event\n")
    } catch (_: Throwable) {
    }
  }

  override fun onResume() {
    super.onResume()
    logLifecycle("onResume")
    Log.i("dsh-diag", "onResume")
  }

  override fun onWindowFocusChanged(hasFocus: Boolean) {
    super.onWindowFocusChanged(hasFocus)
    Log.i("dsh-diag", "onWindowFocusChanged hasFocus=$hasFocus")
  }

  override fun onPause() {
    logLifecycle("onPause")
    Log.i("dsh-diag", "onPause")
    super.onPause()
  }

  override fun onStop() {
    logLifecycle("onStop")
    super.onStop()
  }

  override fun onDestroy() {
    logLifecycle("onDestroy")
    super.onDestroy()
  }

  /**
   * 系统切明暗（含深色模式定时自动切换）时刷新条带。
   * AndroidManifest 的 configChanges 含 uiMode → Activity 不会被重建，
   * 不处理这一条就会出现"页面已换深色、状态栏条带还是浅色"的分层断裂。
   */
  override fun onConfigurationChanged(newConfig: Configuration) {
    super.onConfigurationChanged(newConfig)
    if (!stripOwnedByPage) applyCanvasTheme()
  }

  /**
   * 把条带底色与状态栏图标明暗对齐到当前 canvas 资源。
   * 图标明暗按 canvas 的实际亮度推导（不是按"当前是否深色模式"写死）：
   * 浅底配深色图标、深底配浅色图标，亮度是唯一真值来源，两处不会各说各话。
   */
  private fun applyCanvasTheme() {
    val argb = ContextCompat.getColor(this, R.color.canvas)
    window.decorView.setBackgroundColor(argb)
    WindowCompat.getInsetsController(window, window.decorView).apply {
      isAppearanceLightStatusBars = isLightColor(argb)
    }
  }

  /** 相对亮度（WCAG 线性化 + Rec.709 权重）> 0.5 视为浅底。 */
  private fun isLightColor(argb: Int): Boolean {
    fun lin(channel: Int): Double {
      val c = channel / 255.0
      return if (c <= 0.04045) c / 12.92 else ((c + 0.055) / 1.055).pow(2.4)
    }
    val l = 0.2126 * lin(argb shr 16 and 0xFF) +
      0.7152 * lin(argb shr 8 and 0xFF) +
      0.0722 * lin(argb and 0xFF)
    return l > 0.5
  }

  /** 前端已用 set_status_theme 指定条带底色（进 GUI），连接页明暗跟随逻辑让位。 */
  internal fun markStripOwnedByPage() {
    stripOwnedByPage = true
  }
}

/**
 * 状态栏条带配色切换任务（Handler.post 到主线程执行）。
 * decorView 背景 = 状态栏/刘海安全区条带颜色；APPEARANCE_LIGHT_STATUS_BARS
 * 控制状态栏图标深色（配浅色页面）还是浅色（配深色页面）。
 */
class StatusBarThemeTask(
  private val activity: Activity,
  private val argb: Int,
  private val lightIcons: Boolean,
) : Runnable {
  override fun run() {
    val decor = activity.window.decorView
    decor.setBackgroundColor(argb)
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
      activity.window.insetsController?.setSystemBarsAppearance(
        if (lightIcons) 0 else WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS,
        WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS,
      )
    } else {
      @Suppress("DEPRECATION")
      decor.systemUiVisibility = if (lightIcons) 0 else 0x2000 // LIGHT_STATUS_BAR
    }
    // 这条路径来自页面本身（进 GUI 前前端调用），因此后续系统切明暗
    // 不该再把条带改回连接页底色。
    (activity as? MainActivity)?.markStripOwnedByPage()
  }
}
