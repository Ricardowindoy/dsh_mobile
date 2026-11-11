package com.namelessric.dsh_mobile

import android.content.Context
import android.net.Uri
import java.io.File
import java.net.URI
import org.json.JSONObject

/**
 * RFC 8252 loopback OAuth 回调的 Kotlin 侧改写。
 *
 * 背景（与 docs-repo/docs/loopback-white-screen-postmortem.md 同一结论）：
 * App 的 WebView 跑在手机上，授权页完成后 302 到 `http://127.0.0.1:<port>/...`，
 * 这个 127.0.0.1 指向**手机自己**，而回调服务开在 DSH 服务器上 ⇒ 必然
 * ERR_CONNECTION_REFUSED。这里在 WebView 的 `shouldOverrideUrlLoading`
 * 里把 loopback URL 改写成 relay 的 `/__dsh_loopback__/<port>/...`，
 * 由 dsh-tunnel（--allow-loopback-ports）拨回服务器本机端口。
 *
 * 为什么放在 Kotlin 而不是 Rust `plugin.on_navigation`：
 * - 上次 0.1.16/0.1.17 的 Rust 钩子在 Android 的 `shouldOverrideUrlLoading`
 *   JNI 同步链路上做文件 IO / 跨线程等待，导致主线程 ANR → 白屏；
 * - 这里在 Kotlin 侧先判 URL，再决定是否调用 Rust；回调路径只做内存操作，
 *   不读盘、不跨线程等待。
 *
 * relay 入口在启动时由后台线程从 `config.json` 读一次并缓存；读盘不在
 * `shouldOverrideUrlLoading` 内发生。
 */
internal object LoopbackOAuthRelay {
    @Volatile
    private var relayBase: String? = null

    @Volatile
    private var warming = false

    /** 预热 relay 入口缓存；可重复调用，成功缓存后直接返回。 */
    fun warm(context: Context) {
        if (relayBase != null || warming) return
        warming = true
        val appContext = context.applicationContext ?: context
        Thread {
            try {
                val file = File(appContext.dataDir, "config.json")
                if (!file.isFile) return@Thread
                val raw = JSONObject(file.readText()).optString("relayUrl", "").trim()
                relayBase = normalizeRelayBase(raw)
            } catch (_: Throwable) {
                // 读不到就保持 null：回调走原路径（退化为旧行为），不阻塞页面。
            } finally {
                warming = false
            }
        }.apply {
            name = "dsh-loopback-relay"
            isDaemon = true
        }.start()
    }

    /**
     * 把 `http://127.0.0.1:<port>/<path>?<query>` 改写成
     * `<relay>/__dsh_loopback__/<port>/<path>?<query>`。
     *
     * 只处理 **http + loopback 主机 + 显式端口**；其它一律返回 null 放行。
     * 返回值只由 URL 与内存缓存决定，不触发任何 IO。
     */
    fun rewrite(url: Uri): String? = buildLoopbackUrl(
        relayBase = relayBase,
        scheme = url.scheme,
        host = url.host,
        port = url.port,
        encodedPath = url.encodedPath,
        encodedQuery = url.encodedQuery,
        encodedFragment = url.encodedFragment,
    )
}

/**
 * 纯字符串版本的改写规则（无 Android 依赖，便于 JVM 单测）：
 * 只接受 **http + loopback 主机 + 显式端口**；其它一律 null。
 */
internal fun buildLoopbackUrl(
    relayBase: String?,
    scheme: String?,
    host: String?,
    port: Int,
    encodedPath: String?,
    encodedQuery: String?,
    encodedFragment: String?,
): String? {
    if (scheme != "http") return null
    if (host != "127.0.0.1" && host != "localhost" && host != "::1" && host != "[::1]") return null
    if (port <= 0 || port > 65535) return null
    val base = relayBase ?: return null
    val path = encodedPath ?: "/"
    val query = encodedQuery?.let { "?$it" } ?: ""
    val fragment = encodedFragment?.let { "#$it" } ?: ""
    return "$base/__dsh_loopback__/$port$path$query$fragment"
}

/** 归一化 relay 入口：只接受 http(s)、有 host；去掉 query/fragment/尾斜杠。 */
internal fun normalizeRelayBase(raw: String): String? {
    if (raw.isEmpty()) return null
    val trimmed = raw.substringBefore('#').substringBefore('?').trimEnd('/')
    return try {
        val uri = URI(trimmed)
        val scheme = uri.scheme?.lowercase()
        if (scheme != "http" && scheme != "https") return null
        if (uri.host.isNullOrEmpty()) return null
        trimmed
    } catch (_: Exception) {
        null
    }
}
