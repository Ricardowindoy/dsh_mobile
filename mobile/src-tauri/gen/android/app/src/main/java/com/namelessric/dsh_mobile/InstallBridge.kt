package com.namelessric.dsh_mobile

import android.app.Activity
import android.app.PendingIntent
import android.content.Intent
import android.content.pm.PackageInstaller
import android.net.Uri
import android.os.Build
import android.util.Log
import androidx.core.content.FileProvider
import java.io.File

/**
 * 应用内更新安装桥（由 Rust install_update 经 JNI 调用）。
 *
 * 为什么在 Kotlin 侧做 try/catch：Rust 里逐条 JNI 调用失败时只能拿到 jni
 * crate 的 "Java exception was thrown" 一行，定位不到真实原因（1.0.2–1.0.5
 * 的更新失败全是这样被吞掉的）。Java 侧 catch 后把类名+message 原文带回。
 *
 * 两条安装通道：
 *  1. FileProvider content:// + ACTION_VIEW —— 标准做法，多数 ROM 可用；
 *  2. PackageInstaller 会话 —— 完全绕开 FileProvider（不依赖 provider/
 *     file_paths 配置）。1.0.4 实测 FileProvider.getUriForFile 在目标设备
 *     上必抛异常，所以通道 2 是真正的兜底。
 */
object InstallBridge {
  private const val TAG = "dsh-install"

  /** 返回 null = 安装器已拉起；非 null = 人类可读的失败原因（两条通道都失败时合并）。 */
  @JvmStatic
  fun installUpdate(activity: Activity, path: String): String? {
    val file = File(path)
    if (!file.exists()) return "更新包不存在：$path"
    if (file.length() <= 0L) return "更新包为空文件：$path"

    val fileProviderError = tryFileProvider(activity, file)
    if (fileProviderError == null) {
      Log.i(TAG, "installed via FileProvider action view")
      return null
    }
    Log.w(TAG, "FileProvider path failed: $fileProviderError; falling back to PackageInstaller")

    val packageInstallerError = tryPackageInstaller(activity, file)
    if (packageInstallerError == null) {
      Log.i(TAG, "installed via PackageInstaller session")
      return null
    }
    return "安装失败。FileProvider: $fileProviderError ｜ PackageInstaller: $packageInstallerError"
  }

  /** 返回 null = 已拉起；否则返回失败原因。 */
  private fun tryFileProvider(activity: Activity, file: File): String? {
    return try {
      val authority = "${activity.packageName}.fileprovider"
      val uri: Uri = FileProvider.getUriForFile(activity, authority, file)
      val intent = Intent(Intent.ACTION_VIEW).apply {
        setDataAndType(uri, "application/vnd.android.package-archive")
        addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
      }
      activity.startActivity(intent)
      null
    } catch (e: Throwable) {
      describe(e)
    }
  }

  /** 返回 null = 会话已提交（系统安装器即将弹出）；否则返回失败原因。 */
  private fun tryPackageInstaller(activity: Activity, file: File): String? {
    return try {
      val installer = activity.packageManager.packageInstaller
      val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL)
      params.setAppPackageName(activity.packageName)
      val sessionId = installer.createSession(params)
      installer.openSession(sessionId).use { session ->
        session.openWrite("dsh-update.apk", 0, file.length()).use { out ->
          file.inputStream().use { it.copyTo(out, 64 * 1024) }
          session.fsync(out)
        }
        // 系统把安装结果回送到这个 PendingIntent；指向本应用即可（会带结果
        // extras 回到 MainActivity，不改变现有页面状态）。
        val resultIntent = Intent(activity, MainActivity::class.java).apply {
          action = "com.namelessric.dsh_mobile.INSTALL_RESULT"
          putExtra("dshSessionId", sessionId)
        }
        // FLAG_MUTABLE 是 API 31 才有的常量：低版本传未知 flag 会被
        // PendingIntent 判为非法参数，按版本条件拼接。
        var pendingFlags = PendingIntent.FLAG_UPDATE_CURRENT
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
          pendingFlags = pendingFlags or PendingIntent.FLAG_MUTABLE
        }
        val pending = PendingIntent.getActivity(activity, sessionId, resultIntent, pendingFlags)
        session.commit(pending.intentSender)
      }
      null
    } catch (e: Throwable) {
      describe(e)
    }
  }

  private fun describe(e: Throwable): String {
    val cause = e.cause?.let { "; cause=${it.javaClass.simpleName}: ${it.message}" } ?: ""
    return "${e.javaClass.name}: ${e.message}$cause"
  }
}
