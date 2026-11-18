package com.namelessric.dsh_mobile

import android.os.Handler
import android.os.Looper
import android.webkit.CookieManager
import android.webkit.ValueCallback
import android.webkit.WebStorage
import android.webkit.WebView
import android.webkit.WebViewDatabase

/**
 * 全量清理 App WebView 里的站点数据（cookie / localStorage / IndexedDB /
 * 表单与 HTTP 认证缓存 / 网页缓存），**配对信息不在其中**。
 *
 * ## 为什么需要（与 CookieBridge 的分工）
 *
 * [CookieBridge] 是「按站点清」：站点清单来自 [CookieBridge] 里的已知登录主机
 * 探测（CookieManager 没有枚举 API），所以清单外的站点根本不会出现在面板里，
 * 用户就会遇到「面板说清干净了，打开还是登录态」。而且按站点清只碰 cookie 与
 * web storage，不动 HTTP 缓存、表单数据、HTTP Basic 认证缓存。
 *
 * 这里的 [wipeAll] 是**用户显式点「全部清除」**时才走的另一条路：调用系统
 * 整店 API（`removeAllCookies` / `WebStorage.deleteAllData`），一次清到底。
 * 两者必须分开两个文件：CookieBridge 的不变式是「代码里绝不出现全清调用」
 * （清全部会踢掉 dsh 自己的登录 cookie），那条不变式由单元闸门逐字钉住。
 *
 * ## 配对信息为什么不会丢
 *
 * 配对凭据（relayUrl / deviceToken / deviceId）由 Rust 侧写在 app 配置目录的
 * `config.json` 里，**不在 WebView 存储里**（localStorage 只是镜像）。前端在
 * 清理之前先把这份配置读出来，清完再写回去，然后用设备凭据向 relay 换一次性
 * ticket 重新登录（`pair/refresh` → `login?ticket=`），dsh 的登录 cookie 会被
 * 重新种上 —— 用户不需要重新扫码。
 *
 * ## 复核与诚实
 *
 * `removeAllCookies` 与 `deleteAllData` 都是异步落盘的，所以调用完必须延迟
 * 复查：重新探测已知登录主机，还有 cookie 就如实报出来（[CookieBridge.sites]
 * 的同一套探测），不许把「我调用了 API」当成「已经清干净」。
 */
object WebDataWipe {

    /** 清完等多久再复核（store 落盘 + 页面回写都在这段窗口里发生）。 */
    private const val RECHECK_MS = 600L

    /**
     * 当前 WebView 实例（`MainActivity.onWebViewCreate` 装进来）。
     *
     * 刻意让**依赖只有一个方向**：本类不引用 MainActivity，而是 MainActivity
     * 把 WebView 交进来。这样本文件可以被本地 kotlinc 单独编译（闸门
     * scripts/verify/kotlin-cookie-bridge.sh 就是这么验的），不会因为拖进
     * 整个 Activity 而只能在 CI 上编。
     *
     * 拿不到实例不算致命：cookie / web storage / 表单数据都不需要 WebView
     * 对象，只有「清网页磁盘缓存与历史」那一步用得上；缺了就跳过并在复核里
     * 如实体现（cookie 仍然是清干净的）。
     */
    @Volatile
    private var webViewRef: WebView? = null

    /** 由 MainActivity 在 onWebViewCreate 里调用。 */
    @JvmStatic
    fun attach(view: WebView) {
        webViewRef = view
    }

    /**
     * 全量清理。回调形如：
     * `{"ok":true,"remaining":0,"remainingHosts":[]}`，失败 `{"error":"..."}`。
     *
     * 注意：`ok` 只在复查确实没有残留 cookie 时才为 true。
     */
    @JvmStatic
    fun wipeAll(callback: ValueCallback<String>) {
        runOnMain {
            try {
                val manager = CookieManager.getInstance()
                val webView = webViewRef
                val handler = Handler(Looper.getMainLooper())
                // removeAllCookies 的回调在个别 ROM/WebView 组合上可能不回来
                // （1.0.28 之前 setCookie 的回调就发生过）。回调不来时不能卡死：
                // 3s 后照常往下走，把「没等到回调」如实标进结果，让前端知道
                // 这次复核的可信度打了折扣 —— 而不是报一句"超时"了事。
                var advanced = false
                fun advance(waited: Boolean) {
                    if (advanced) return
                    advanced = true
                    clearStorageAndCaches(manager, webView, waited, callback)
                }
                handler.postDelayed({ advance(false) }, 3000L)
                manager.removeAllCookies { _ -> advance(true) }
            } catch (t: Throwable) {
                callback.onReceiveValue(CookieBridge.errorJson("wipeAll", t))
            }
        }
    }

    private fun clearStorageAndCaches(
        manager: CookieManager,
        webView: WebView?,
        cookieCallbackCame: Boolean,
        callback: ValueCallback<String>,
    ) {
        var storageCleared = false
        var formCleared = false
        try {
            // 所有源的 localStorage / IndexedDB / WebSQL，一次清完。
            WebStorage.getInstance().deleteAllData()
            storageCleared = true
        } catch (_: Throwable) {
            // 失败不致命：cookie 已清，storage 残留会在复核里如实反映。
        }
        try {
            val ctx = webView?.context
            if (ctx != null) {
                val db = WebViewDatabase.getInstance(ctx)
                db.clearFormData()
                db.clearHttpAuthUsernamePassword()
                db.clearUsernamePassword()
                formCleared = true
            }
        } catch (_: Throwable) {
            // 同上：都不影响"登录 cookie 已清"这个主结论。
        }
        try {
            // 3) 网页磁盘缓存 + 历史：GUI 的 boot 页与插件 bundle 都走缓存，
            //    不清会出现"数据清了、页面还是旧的"。clearHistory 顺手把退回到
            //    已清页面的后门关掉。
            webView?.clearCache(true)
            webView?.clearHistory()
        } catch (_: Throwable) {
        }
        try { manager.flush() } catch (_: Throwable) { }

        // 4) 延迟复核：只剩 cookie 能可靠复查，storage 没有公开的枚举 API。
        val handler = Handler(Looper.getMainLooper())
        handler.postDelayed({
            try {
                val left = CookieBridge.hostsWithCookies(manager)
                val hosts = left.joinToString(",") { "\"" + jsonEscape(it) + "\"" }
                callback.onReceiveValue(
                    "{\"ok\":${left.isEmpty()}," +
                        "\"storageCleared\":$storageCleared,\"formCleared\":$formCleared," +
                        "\"cookieCallbackCame\":$cookieCallbackCame," +
                        "\"remaining\":${left.size},\"remainingHosts\":[$hosts]}",
                )
            } catch (t: Throwable) {
                callback.onReceiveValue(CookieBridge.errorJson("wipeAll.verify", t))
            }
        }, RECHECK_MS)
    }

    private fun runOnMain(block: () -> Unit) {
        if (Looper.myLooper() == Looper.getMainLooper()) block()
        else Handler(Looper.getMainLooper()).post(block)
    }

    private fun jsonEscape(s: String): String = buildString(s.length) {
        for (c in s) {
            when (c) {
                '"' -> append("\\\"")
                '\\' -> append("\\\\")
                '\n' -> append("\\n")
                '\r' -> append("\\r")
                '\t' -> append("\\t")
                else -> if (c < ' ') append("\\u%04x".format(c.code)) else append(c)
            }
        }
    }
}
