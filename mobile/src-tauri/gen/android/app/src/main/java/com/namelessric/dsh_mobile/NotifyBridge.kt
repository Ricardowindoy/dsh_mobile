package com.namelessric.dsh_mobile

import android.Manifest
import android.app.Activity
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.PowerManager
import android.provider.Settings
import android.os.Build
import android.util.Log
import androidx.core.app.ActivityCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import org.json.JSONArray
import org.json.JSONObject
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Executors

/**
 * 审批/提问通知卡片（WP4c）。
 *
 * 生命周期：Rust 侧 notify_start 经 JNI 调 start()（SSE 订阅在 Rust 维持，
 * 每条消息经 onCard 推到这里）；notify_stop 调 stop()。进程被杀后无通知
 * —— v1 不做常驻服务（电池友好），这是文档写明的已知限制。
 *
 * 安全：卡片只是提示；「允许一次/拒绝」action 由 Rust 用设备 token 走
 * header POST 到 relay（nativeAnswer），Kotlin 不持有也不转发任何凭据到
 * 别处。decision 闭集在 Rust/relay 两侧各校验一次。
 */
object NotifyBridge {
  private const val TAG = "dsh-notify"
  private const val CHANNEL_ID = "dsh-approvals"
  private const val REQUEST_POST_NOTIFICATIONS = 1001
  private const val ACTION_ANSWER = "com.namelessric.dsh_mobile.NOTIFY_ANSWER"
  private const val EXTRA_ID = "id"
  private const val EXTRA_DECISION = "decision"
  /** Structured question answer as JSON: [{"id":"scope","selected":["方案 A"]}]. */
  private const val EXTRA_ANSWERS = "answersJson"

  private val executor = Executors.newSingleThreadExecutor()
  private val cards = ConcurrentHashMap<String, JSONObject>()

  /** 已处理过的卡片（id@ts），LRU 上限 256：relay 重放时兜底去重。 */
  private val seen = object : LinkedHashMap<String, Boolean>(256, 0.75f, true) {
    override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, Boolean>?) = size > 256
  }

  /** 通知链路诊断日志（cacheDir/dsh-notify.log）：只在出问题时看，64KB 封顶。 */
  private fun logLine(ctx: Context, msg: String) {
    try {
      val f = java.io.File(ctx.cacheDir, "dsh-notify.log")
      if (f.length() > 64 * 1024) f.delete()
      f.appendText("${System.currentTimeMillis()} $msg\n")
    } catch (_: Throwable) {
    }
  }
  @Volatile private var started = false
  @Volatile private var appContext: Context? = null

  // Rust 实现的应答发送（reqwest + 私有 CA + 设备 token header）。
  external fun nativeAnswer(id: String, decision: String)

  // Rust 实现的结构化提问应答（question 卡的一键选项）。
  external fun nativeAnswerJson(id: String, answersJson: String)

  /** JNI 入口：Rust notify_start 调用。 */
  @JvmStatic
  fun start(context: Context) {
    appContext = context.applicationContext
    val ctx = appContext ?: return
    // Android 13+：POST_NOTIFICATIONS 是运行时权限，manifest 声明不等于已授予。
    // 不主动请求时 NotificationManagerCompat.notify 只会抛 SecurityException，
    // 旧代码在 showCard 里静默降级，用户会以为「通知坏了」。这里在订阅启动时
    // 用 Activity 请求一次；用户拒绝后仍可到系统设置里手动打开。
    if (Build.VERSION.SDK_INT >= 33) {
      val activity = context as? Activity
      if (activity != null &&
        ContextCompat.checkSelfPermission(activity, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
        activity.runOnUiThread {
          ActivityCompat.requestPermissions(
            activity,
            arrayOf(Manifest.permission.POST_NOTIFICATIONS),
            REQUEST_POST_NOTIFICATIONS,
          )
        }
      }
    }
    val permState = if (Build.VERSION.SDK_INT >= 33) {
      if (ContextCompat.checkSelfPermission(ctx, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED) "granted" else "denied"
    } else "n/a"
    val enabled = NotificationManagerCompat.from(ctx).areNotificationsEnabled()
    logLine(ctx, "start perm=$permState notificationsEnabled=$enabled")
    if (started) return
    started = true
    val nm = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
    if (Build.VERSION.SDK_INT >= 26) {
      val channel = NotificationChannel(CHANNEL_ID, "审批与提问", NotificationManager.IMPORTANCE_HIGH).apply {
        description = "agent 等待你决定时推送；可直接允许一次或拒绝"
        enableVibration(true)
      }
      nm.createNotificationChannel(channel)
    }
    // 常驻前台服务：让 SSE 订阅在 App 切后台后继续存活（v1.0.15）。
    NotifyService.start(ctx)
    // Doze 会掐断后台网络：首次启动时请求一次「忽略电池优化」，否则前台服务
    // 也保不住 SSE（用户实测：切前台才补弹通知）。
    requestIgnoreBatteryOptimizations(context, ctx)
    logLine(ctx, "keepalive start requested running=${NotifyService.running}")
    Log.i(TAG, "started")
  }

  /** JNI 入口：Rust notify_stop 调用。撤下所有未决卡片。 */
  @JvmStatic
  fun stop() {
    started = false
    val ctx = appContext ?: return
    val nm = NotificationManagerCompat.from(ctx)
    for (id in cards.keys) nm.cancel(id.hashCode())
    cards.clear()
    NotifyService.stop(ctx)
    Log.i(TAG, "stopped")
  }

  /** JNI 入口：一条 SSE 消息（JSON 原文）。kind=approval|question|done|retract。 */
  @JvmStatic
  fun onCard(json: String) {
    if (!started) return
    val ctx = appContext ?: return
    executor.execute {
      try {
        val msg = JSONObject(json)
        val id = msg.optString("id")
        if (id.isEmpty()) return@execute
        // 同一张卡（id 相同、ts 相同）只处理一次。done 卡用稳定 id 覆盖更新，
        // ts 会变，所以必须带 ts 才能既去重又不挡正常更新。
        val dedupeKey = "$id@${msg.optLong("ts")}"
        if (seen.containsKey(dedupeKey)) return@execute
        seen[dedupeKey] = true
        when (msg.optString("kind")) {
          "retract" -> {
            cards.remove(id)
            NotificationManagerCompat.from(ctx).cancel(id.hashCode())
          }
          "approval", "question" -> {
            cards[id] = msg
            logLine(ctx, "card kind=${msg.optString("kind")} id=$id tool=${msg.optString("toolName")}")
            showCard(ctx, id, msg)
          }
          "done" -> {
            // 完成卡是提示，不是待办：不进 cards（无需 retract），也不带 action。
            logLine(ctx, "card kind=done id=$id title=${msg.optString("title")}")
            showCard(ctx, id, msg)
          }
        }
      } catch (e: Exception) {
        Log.w(TAG, "onCard: ${e.message}")
      }
    }
  }

  /**
   * 请求把应用加入电池优化白名单（一次性）。
   *
   * Android 的 Doze 对「非白名单应用」会推迟网络访问——前台服务并不能完全
   * 豁免。不申请的话，App 挂后台一段时间后 SSE 必然断，表现就是「切回前台
   * 才一次性弹出所有通知」。系统会弹一次确认框，用户拒绝也能继续用（只是
   * 后台可靠性差），不阻塞任何流程。
   */
  private fun requestIgnoreBatteryOptimizations(context: Context, appCtx: Context) {
    try {
      val pm = appCtx.getSystemService(Context.POWER_SERVICE) as PowerManager
      if (pm.isIgnoringBatteryOptimizations(appCtx.packageName)) return
      val prefs = appCtx.getSharedPreferences("dsh-notify", Context.MODE_PRIVATE)
      if (prefs.getBoolean("batteryPromptDone", false)) return
      val activity = context as? Activity ?: return
      prefs.edit().putBoolean("batteryPromptDone", true).apply()
      activity.runOnUiThread {
        try {
          activity.startActivity(
            Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
              Uri.parse("package:${appCtx.packageName}")),
          )
        } catch (e: Throwable) {
          logLine(appCtx, "battery prompt failed: ${e.message}")
        }
      }
    } catch (e: Throwable) {
      logLine(appCtx, "battery prompt error: ${e.message}")
    }
  }

  private fun showCard(ctx: Context, id: String, msg: JSONObject) {
    val kind = msg.optString("kind")
    val tool = msg.optString("toolName")
    val reason = msg.optString("reason")
    val sessionTitle = msg.optString("title")
    val question = msg.optString("question")
    val preview = msg.optString("preview")
    val status = msg.optString("status")

    // 会话标题优先放进标题行，让锁屏上就能分辨是哪个会话。
    // 中断/出错/阻塞与正常完成共用 done 通道，靠 status 区分措辞。
    val doneLabel = when (status) {
      "interrupted" -> "已中断"
      "error" -> "出错"
      "blocked" -> "被阻塞"
      else -> "完成"
    }
    val title = when (kind) {
      "approval" -> if (sessionTitle.isNotEmpty()) "审批 · $sessionTitle" else "审批等待：$tool"
      "question" -> if (sessionTitle.isNotEmpty()) "提问 · $sessionTitle" else "dsh 有新提问"
      else -> if (sessionTitle.isNotEmpty()) "$doneLabel · $sessionTitle" else "dsh $doneLabel"
    }
    val text = when (kind) {
      "approval" -> reason.ifEmpty { "点击查看并处理" }
      "question" -> question.ifEmpty { "点击查看并处理" }
      else -> preview.ifEmpty { when (status) {
        "interrupted" -> "会话已中断"
        "error" -> "本轮执行出错"
        "blocked" -> "本轮被阻塞"
        else -> "点击查看结果"
      } }
    }

    val open = Intent(ctx, MainActivity::class.java).apply {
      flags = Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP
    }
    val openPi = PendingIntent.getActivity(ctx, id.hashCode(), open,
      PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)

    // 审批卡在展开态补一行超时说明（设计 04 ① 的脚注）；收起态仍只显示 reason。
    val bigText = if (kind == "approval") {
      "$text\n110 秒内无人应答将自动拒绝（沉默不是同意）"
    } else text
    val builder = NotificationCompat.Builder(ctx, CHANNEL_ID)
      .setSmallIcon(android.R.drawable.ic_dialog_info)
      .setContentTitle(title)
      .setContentText(text)
      .setStyle(NotificationCompat.BigTextStyle().bigText(bigText))
      .setContentIntent(openPi)
      .setAutoCancel(true)
      .setPriority(NotificationCompat.PRIORITY_HIGH)
      .setCategory(NotificationCompat.CATEGORY_REMINDER)

    // 完成卡每个会话只有一个槽位（插件用稳定 id）：后续更新只替换内容，
    // 不再响铃/震动，避免「每回一句就叮一下」。
    if (kind == "done") builder.setOnlyAlertOnce(true)

    if (kind == "approval") {
      builder.addAction(0, "允许一次", answerPi(ctx, id, "allowed-once"))
      builder.addAction(0, "拒绝", answerPi(ctx, id, "rejected"))
    } else if (kind == "question") {
      // 一键选项：插件只在「单题、单选、1..3 个选项」时下发 options；
      // 其余提问 options 为空，点击卡片进 App 里作答。
      val qid = msg.optString("questionId")
      val opts = msg.optJSONArray("options")
      if (qid.isNotEmpty() && opts != null) {
        for (i in 0 until minOf(opts.length(), 3)) {
          val label = opts.optString(i)
          if (label.isNotEmpty()) {
            val shown = if (label.length <= 24) label else label.take(24) + "…"
            builder.addAction(0, shown, questionAnswerPi(ctx, id, qid, label))
          }
        }
      }
    }

    try {
      NotificationManagerCompat.from(ctx).notify(id.hashCode(), builder.build())
    } catch (e: SecurityException) {
      // POST_NOTIFICATIONS 被拒：降级为仅 App 内可见（cards 表仍在）。
      Log.w(TAG, "notification permission denied: ${e.message}")
    }
  }

  private fun answerPi(ctx: Context, id: String, decision: String): PendingIntent {
    val intent = Intent(ctx, AnswerReceiver::class.java).apply {
      action = ACTION_ANSWER
      putExtra(EXTRA_ID, id)
      putExtra(EXTRA_DECISION, decision)
    }
    return PendingIntent.getBroadcast(ctx, (id + decision).hashCode(), intent,
      PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
  }

  /** One-tap question action: structured answer for a single question id. */
  private fun questionAnswerPi(ctx: Context, id: String, qid: String, label: String): PendingIntent {
    val answers = JSONArray().put(
      JSONObject().put("id", qid).put("selected", JSONArray().put(label)),
    ).toString()
    val intent = Intent(ctx, AnswerReceiver::class.java).apply {
      action = ACTION_ANSWER
      putExtra(EXTRA_ID, id)
      putExtra(EXTRA_ANSWERS, answers)
    }
    return PendingIntent.getBroadcast(ctx, (id + qid + label).hashCode(), intent,
      PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
  }

  /** action 按钮接收器：撤卡 → 交 Rust 发应答 → 结果更新为已处理提示。 */
  class AnswerReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
      if (intent.action != ACTION_ANSWER) return
      val id = intent.getStringExtra(EXTRA_ID) ?: return
      val answers = intent.getStringExtra(EXTRA_ANSWERS)
      val decision = intent.getStringExtra(EXTRA_DECISION)
      // Exactly one answer shape, mirroring the relay gate: structured question
      // answers, or an approval decision from the closed set.
      if (answers != null) {
        cards.remove(id)
        NotificationManagerCompat.from(context).cancel(id.hashCode())
        executor.execute {
          try {
            nativeAnswerJson(id, answers)
          } catch (e: Exception) {
            Log.w(TAG, "question answer failed: ${e.message}")
          }
        }
        return
      }
      if (decision != "allowed-once" && decision != "rejected") return
      cards.remove(id)
      NotificationManagerCompat.from(context).cancel(id.hashCode())
      executor.execute {
        try {
          nativeAnswer(id, decision)
        } catch (e: Exception) {
          Log.w(TAG, "answer failed: ${e.message}")
        }
      }
    }
  }
}