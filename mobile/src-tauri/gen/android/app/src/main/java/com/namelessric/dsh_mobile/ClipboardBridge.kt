package com.namelessric.dsh_mobile

import android.app.Activity
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.util.Base64
import android.util.Log
import org.json.JSONObject
import java.io.ByteArrayOutputStream

/**
 * 剪贴板读取桥（Rust read_clipboard 经 JNI 调用）。
 *
 * 为什么需要原生读：Android WebView 的 navigator.clipboard.read() 在没有
 * 显式用户授权的情况下直接抛 "Read permission denied"（1.0.4 实测），
 * readText() 又只拿得到纯文本 —— 用户复制一张二维码截图后「粘贴配对」
 * 必然失败。ClipboardManager 属于应用可直接读的系统服务，不受 WebView
 * 剪贴板策略影响。
 *
 * 返回 JSON：
 *   {"type":"empty"}
 *   {"type":"text","text":"..."}
 *   {"type":"image","mime":"image/png","base64":"..."}
 *   {"type":"error","message":"..."}
 */
object ClipboardBridge {
  private const val TAG = "dsh-clipboard"
  private const val MAX_IMAGE_BYTES = 12 * 1024 * 1024

  @JvmStatic
  fun readClipboard(activity: Activity): String {
    return try {
      val manager = activity.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager
      val clip: ClipData? = manager.primaryClip
      if (clip == null || clip.itemCount == 0) return JSONObject().put("type", "empty").toString()

      var text: String? = null
      for (i in 0 until clip.itemCount) {
        val item = clip.getItemAt(i)
        // 图片优先：截图复制过来的是 content:// URI，文本通常为空。
        val uri = item.uri
        if (uri != null) {
          val mime = activity.contentResolver.getType(uri) ?: clip.description.getMimeType(0)
          if (mime != null && mime.startsWith("image/")) {
            val bytes = activity.contentResolver.openInputStream(uri)?.use { input ->
              val buf = ByteArrayOutputStream()
              val chunk = ByteArray(64 * 1024)
              var total = 0
              while (true) {
                val n = input.read(chunk)
                if (n <= 0) break
                total += n
                if (total > MAX_IMAGE_BYTES) return JSONObject()
                  .put("type", "error")
                  .put("message", "剪贴板图片超过 12MB，请改用「相册识别」选图")
                  .toString()
                buf.write(chunk, 0, n)
              }
              buf.toByteArray()
            }
            if (bytes != null && bytes.isNotEmpty()) {
              return JSONObject()
                .put("type", "image")
                .put("mime", mime)
                .put("base64", Base64.encodeToString(bytes, Base64.NO_WRAP))
                .toString()
            }
          }
        }
        val t = item.text?.toString()
        if (!t.isNullOrBlank()) text = t
      }
      if (!text.isNullOrBlank()) {
        JSONObject().put("type", "text").put("text", text).toString()
      } else {
        JSONObject().put("type", "empty").toString()
      }
    } catch (e: Throwable) {
      Log.e(TAG, "read clipboard failed", e)
      JSONObject()
        .put("type", "error")
        .put("message", "${e.javaClass.simpleName}: ${e.message}")
        .toString()
    }
  }
}
