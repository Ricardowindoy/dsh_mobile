package com.namelessric.dsh_mobile

import android.webkit.ValueCallback

/**
 * Rust 侧经 JNI 调用的入口（MainActivity companion external fun 的承载者）。
 *
 * JNI 只能直接调 static 方法；CookieBridge 的回调形态（ValueCallback<String>）
 * 在 JNI 侧构造 Java 回调对象成本高，所以这里给一个**同步阻塞**的薄壳：
 * 在调用线程 latch 等主线程回调返回。Tauri 命令本身是 async 的，Rust 侧会
 * spawn_blocking，不会堵主线程。
 */
object CookieBridgeNative {

    /** 返回 JSON 数组字符串：[{site, cookieCount, names[]}]。 */
    @JvmStatic
    fun listSites(): String {
        var out = "[]"
        val latch = java.util.concurrent.CountDownLatch(1)
        CookieBridge.sites(ValueCallback { v ->
            out = v ?: "[]"
            latch.countDown()
        })
        latch.await(5, java.util.concurrent.TimeUnit.SECONDS)
        return out
    }

    /** 清一个站点；返回 JSON {requested, cleared}。 */
    @JvmStatic
    fun clearSite(site: String, cookieNamesJson: String): String {
        // cookieNamesJson 是前端传来的 JSON 数组字符串；这里最小解析——
        // 去掉括号按逗号切、剥引号。名字来自前端白名单（站点 cookie 的
        // name），不需要完整 JSON 解析器。
        val names = cookieNamesJson.trim()
            .removeSurrounding("[", "]")
            .split(',')
            .mapNotNull { it.trim().removeSurrounding("\"").takeIf { n -> n.isNotEmpty() } }
        var out = "{}"
        val latch = java.util.concurrent.CountDownLatch(1)
        CookieBridge.clearSite(site, names.toTypedArray(), ValueCallback { v ->
            out = v ?: "{}"
            latch.countDown()
        })
        latch.await(5, java.util.concurrent.TimeUnit.SECONDS)
        return out
    }
}
