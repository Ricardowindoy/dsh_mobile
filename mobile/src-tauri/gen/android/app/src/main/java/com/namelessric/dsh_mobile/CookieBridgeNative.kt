package com.namelessric.dsh_mobile

import android.webkit.ValueCallback

/**
 * Rust 侧经 JNI 调用的入口（MainActivity companion external fun 的承载者）。
 *
 * JNI 只能直接调 static 方法；CookieBridge 的回调形态（ValueCallback<String>）
 * 在 JNI 侧构造 Java 回调对象成本高，所以这里给一个**同步阻塞**的薄壳：
 * 在调用线程 latch 等主线程回调返回。Tauri 命令本身在异步运行时线程上跑，
 * 不会堵 UI 线程；若本来就已在主线程，CookieBridge 会就地执行、不会等锁。
 *
 * ## 绝对不许把异常抛回 Rust
 *
 * 1.0.24 真机上这里抛了异常，Rust 侧只收到一句
 * "Java exception was raised during method invocation"，没有任何类名/消息，
 * 现场无法定位。所以两个入口都：
 *   · 整体 try/catch(Throwable)，异常转成 {"error": "类名: 消息"} 返回；
 *   · 超时也返回 {"error": "timeout"} —— 而不是空列表（空列表会被 UI 当成
 *     「没有登录站点」，那是在撒谎）。
 */
object CookieBridgeNative {

    /** 返回 JSON：成功为数组 [{site,cookieCount,names[]}]，失败为 {"error": "..."}。 */
    @JvmStatic
    fun listSites(): String {
        var out = "[]"
        val latch = java.util.concurrent.CountDownLatch(1)
        return try {
            CookieBridge.sites(ValueCallback { v ->
                out = v ?: "[]"
                latch.countDown()
            })
            if (!latch.await(15, java.util.concurrent.TimeUnit.SECONDS)) {
                return "{\"error\":\"扫描超时（主线程 15s 未回调）\"}"
            }
            out
        } catch (t: Throwable) {
            CookieBridge.errorJson("listSites", t)
        }
    }

    /** 返回 JSON：成功为 {requested, cleared}，失败为 {"error": "..."}。 */
    @JvmStatic
    fun clearSite(site: String, cookieNamesJson: String): String {
        var out = "{}"
        val latch = java.util.concurrent.CountDownLatch(1)
        return try {
            // cookieNamesJson 是前端传来的 JSON 数组字符串；这里最小解析——
            // 去掉括号按逗号切、剥引号。名字来自站点 cookie 的 name 白名单，
            // 不需要完整 JSON 解析器。
            val names = cookieNamesJson.trim()
                .removeSurrounding("[", "]")
                .split(',')
                .mapNotNull { it.trim().removeSurrounding("\"").takeIf { n -> n.isNotEmpty() } }
                .toTypedArray()
            CookieBridge.clearSite(site, names, ValueCallback { v ->
                out = v ?: "{}"
                latch.countDown()
            })
            if (!latch.await(15, java.util.concurrent.TimeUnit.SECONDS)) {
                return "{\"error\":\"清除超时（主线程 15s 未回调）\"}"
            }
            out
        } catch (t: Throwable) {
            CookieBridge.errorJson("clearSite", t)
        }
    }
}
