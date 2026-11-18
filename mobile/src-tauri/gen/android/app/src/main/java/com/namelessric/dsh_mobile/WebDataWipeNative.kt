package com.namelessric.dsh_mobile

import android.webkit.ValueCallback

/**
 * Rust 侧经 JNI 调用的全量清理入口（与 [CookieBridgeNative] 同一套薄壳写法）。
 *
 * JNI 只能调 static 方法，而 [WebDataWipe.wipeAll] 的回调形态在 JNI 侧构造
 * Java 回调成本高，所以这里给一个**同步阻塞**的薄壳：在调用线程 latch 等主线程
 * 回调返回。Tauri 命令本身跑在异步运行时线程上，不会堵 UI 线程。
 *
 * 两条与 1.0.24 事故相同的铁律（照抄 CookieBridgeNative，别简化）：
 *   · 整体 catch(Throwable)，异常转成 {"error": "类名: 消息"} —— jni crate 报
 *     JavaException 时不带类名与消息，抛过 JNI 等于现场失明；
 *   · 超时返回 {"error": "timeout"}，绝不用"清完了"或空结果冒充成功。
 *
 * 超时额度给 20s（比清理命令的 15s 宽）：内部最坏情况是 3s 等 cookie 回调
 * 超时 + 1.5s 两段复核窗口，加上主线程排队，15s 会偏紧；这里等不到就明确报
 * 超时，比让用户看到「清理成功但仍是登录态」好。
 */
object WebDataWipeNative {

    /** 返回 JSON：成功为复核结果，失败为 {"error": "..."}。 */
    @JvmStatic
    fun wipeAll(): String {
        var out = ""
        val latch = java.util.concurrent.CountDownLatch(1)
        return try {
            WebDataWipe.wipeAll(ValueCallback { v ->
                out = v ?: ""
                latch.countDown()
            })
            if (!latch.await(20, java.util.concurrent.TimeUnit.SECONDS)) {
                return "{\"error\":\"清理超时（主线程 20s 未回调）\"}"
            }
            if (out.isEmpty()) {
                // 回调给了空串：没拿到复核结果，就不能说清干净了。
                return "{\"error\":\"清理回调返回空结果\"}"
            }
            out
        } catch (t: Throwable) {
            CookieBridge.errorJson("wipeAll", t)
        }
    }
}
