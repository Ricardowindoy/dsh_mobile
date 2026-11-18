package com.namelessric.dsh_mobile

import android.os.Handler
import android.os.Looper
import android.webkit.CookieManager
import android.webkit.WebStorage
import android.webkit.ValueCallback
import java.util.concurrent.atomic.AtomicInteger

/**
 * 按站点清理 App WebView 里的登录 cookie。
 *
 * ## 背景
 *
 * GUI 的内嵌浏览器面板里登录 JetHub 插件跳转的外部站点后，登录态落在本进程
 * WebView 的 cookie store（系统 [CookieManager]）里。切换账号需要按站点清 ——
 * 全清会把 dsh relay 自己的登录态一起杀掉，所以按站点。
 *
 * ## 为什么走原生而不是 WebView JS
 *
 * `document.cookie` 读不到 HttpOnly cookie，也删不掉；[CookieManager] 是唯一
 * 能完整操作这个 store 的入口。
 *
 * ## 站点清单从哪来（不是拍脑袋）
 *
 * [LOGIN_HOSTS] 逐个取自 JetHub 插件里**真实会打开**的登录地址。清单只用于
 * 「探测」，不用于「展示」：只有真查到 cookie 的域才进列表 —— 列一个空站点
 * 等于撒谎。
 *
 * ## 删除为什么这么绕（两条真机教训，别简化）
 *
 * 1. `setCookie(url, value)` 是**异步**的，返回不代表写入生效。1.0.26 的日志里
 *    「cleared=121」而重扫计数纹丝不动，就是拿调用次数当成功次数。必须用
 *    带 callback 的重载等回调结束，再 `flush()`，**然后重新探测复核**。
 * 2. cookie 的身份是 (name, domain, path, secure)。`getCookie` 只返回 name=value，
 *    拿不到 domain/path/secure，所以只能把「host-only / 各级祖先 domain」×
 *    「secure / 非 secure」都写过一遍，再复核到底删掉没有。
 *    ⚠️ 另外别拼 `https://".$host"/` 这种带前导点的 URL —— 那是**非法主机名**，
 *    getCookie/setCookie 都会静默失效（1.0.24 的变体写法就踩了这个）。
 */
object CookieBridge {

    /**
     * JetHub 实际会打开的外部登录主机（含 SSO 子域：cookie 常挂在子域上，
     * 只探 apex 会漏 —— 例如 auth.huaweicloud.com 与 codearts.huaweicloud.com
     * 是两份 host-only cookie）。
     */
    private val LOGIN_HOSTS = arrayOf(
        // Trae / CodeBuddy / WorkBuddy
        "www.trae.cn", "trae.cn", "www.codebuddy.cn", "codebuddy.cn",
        "www.workbuddy.cn", "www.workbuddy.ai", "workbuddy.ai",
        // 华为云 / CodeArts
        "auth.huaweicloud.com", "codearts.huaweicloud.com", "myhuaweicloud.com",
        // 讯飞
        "sso.xfyun.cn", "agent.xfyun.cn", "loomy.xunfei.cn",
        // Z.ai（zcode / chat）
        "chat.z.ai", "zcode.z.ai", "z.ai",
        // 模型社区与账号
        "modelscope.cn", "www.modelscope.cn", "account.minimax.cn",
        "bigmodel.cn", "qoder.cn", "gateway.qoder.com.cn",
        // 七牛 / 其它合作方
        "portal.qiniu.com", "www.magicrew.ai", "codely.tuanjie.cn",
        "www.accio-ai.com", "monkeycode-ai.com", "www.letsmagic.cn",
        "ai-models.app.baizhi.cloud", "copilot.tencent.com",
        // OAuth 提供方
        "open.weixin.qq.com", "accounts.google.com", "opencode.ai",
    )

    /** dsh 自身的登录 cookie 前缀 —— 任何情况下都不许清。 */
    private val PROTECTED_PREFIXES = arrayOf("dsh_relay_device", "dsh_relay_token", "dsh-auth-")

    private fun isProtected(name: String): Boolean {
        val n = name.lowercase()
        return PROTECTED_PREFIXES.any { n.startsWith(it) }
    }

    /**
     * 列出 cookie store 里实际**有 cookie** 的外部登录站点。
     * 回调形如 [{site, cookieCount, names}]。
     */
    @JvmStatic
    fun sites(callback: ValueCallback<String>) {
        runOnMain {
            try {
                val manager = CookieManager.getInstance()
                val out = StringBuilder("[")
                var first = true
                for (host in LOGIN_HOSTS) {
                    val names = probeNames(manager, host)
                    if (names.isEmpty()) continue
                    if (!first) out.append(',')
                    first = false
                    out.append("{\"site\":\"").append(jsonEscape(host))
                        .append("\",\"cookieCount\":").append(names.size)
                        .append(",\"names\":[")
                        .append(names.joinToString(",") { "\"" + jsonEscape(it) + "\"" })
                        .append("]}")
                }
                out.append("]")
                callback.onReceiveValue(out.toString())
            } catch (t: Throwable) {
                callback.onReceiveValue(errorJson("sites", t))
            }
        }
    }

    /**
     * 清掉 [site] 的 cookie，**并在写完之后重新探测复核**。
     *
     * 回调：{"attempts":n,"targeted":n,"cleared":n,"remaining":n,"remainingNames":[...]}
     * cleared/remaining 是复核结果，不是尝试次数 —— 前端据此如实展示。
     *
     * @param cookieNames 只清这些名字；空 = 该 host 上全部（受保护项除外）。
     */
    @JvmStatic
    fun clearSite(site: String, cookieNames: Array<String>, callback: ValueCallback<String>) {
        runOnMain {
            try {
                val manager = CookieManager.getInstance()
                val hosts = probeHostsFor(site)
                val before = hosts.flatMap { probeNames(manager, it) }.toSet()
                // 只清调用方点名的那几枚；没点名就清该 host 全部。
                val targets = if (cookieNames.isEmpty()) before
                else before.filter { cookieNames.contains(it) }.toSet().ifEmpty { cookieNames.toSet() }
                if (targets.isEmpty()) {
                    callback.onReceiveValue(
                        "{\"attempts\":0,\"targeted\":0,\"cleared\":0,\"remaining\":0,\"remainingNames\":[]}",
                    )
                    return@runOnMain
                }

                // 两阶段：先按最常见形态（host-only、非 secure、Path=/）写一遍，
                // 复核若已清干净就收工；还有剩的才升级到「各级祖先 domain ×
                // secure 与否」的全矩阵。一上来就打全矩阵，几百次写入会把等待
                // 预算撑爆，而绝大多数 cookie 其实第一轮就没了。
                var attempts = 0

                fun submit(plans: List<Quad>, next: () -> Unit) {
                    val pending = AtomicInteger(0)
                    for (pl in plans) {
                        val value = buildString {
                            append(pl.name)
                                .append("=; Path=/; Max-Age=0; ")
                                .append("Expires=Thu, 01 Jan 1970 00:00:00 GMT")
                            if (pl.secure) append("; Secure")
                            if (pl.domain != null) append("; Domain=").append(pl.domain)
                        }
                        attempts++
                        pending.incrementAndGet()
                        try {
                            // 带 callback 的重载：写入是异步的，必须等回调 ——
                            // 否则 flush() 可能落盘的是"还没改"的状态。
                            manager.setCookie("https://${pl.host}/", value) {
                                pending.decrementAndGet()
                            }
                        } catch (_: Throwable) {
                            pending.decrementAndGet()
                        }
                    }
                    awaitWritesThen(pending, next)
                }

                val primaryHost = hosts.first()
                val phaseA = targets.map { Quad(primaryHost, it, null, false) }
                submit(phaseA) {
                    try { manager.flush() } catch (_: Throwable) { /* 不致命 */ }
                    val remain1 = targets.intersect(hosts.flatMap { probeNames(manager, it) }.toSet())
                    if (remain1.isEmpty()) {
                        // ⚠️ 先别急着报成功：站点页面若还开着，它的 JS 可能在我们
                        // 写完的几毫秒内又把 cookie 写回来（qoder.cn 就是这么卡住 1 枚的，
                        // 9 次写入全"成功"但复核永远还在）。隔一会儿再查一次再说。
                        lateCheck(manager, hosts, targets, attempts, { plans, next -> submit(plans, next) }, callback)
                        return@submit
                    }
                    val domains = domainCandidatesFor(site)
                    val phaseB = ArrayList<Quad>()
                    for (h in hosts) {
                        for (n in remain1) {
                            for (d in domains) {
                                for (sec in SECURE_VARIANTS) phaseB.add(Quad(h, n, d, sec))
                            }
                        }
                    }
                    submit(phaseB) {
                        try { manager.flush() } catch (_: Throwable) { /* 不致命 */ }
                        lateCheck(manager, hosts, targets, attempts, { plans, next -> submit(plans, next) }, callback)
                    }
                }
            } catch (t: Throwable) {
                callback.onReceiveValue(errorJson("clearSite", t))
            }
        }
    }

    /** 一次写入尝试：往 host 写一个 name 的过期 cookie，带 domain/secure 变体。 */
    private class Quad(val host: String, val name: String, val domain: String?, val secure: Boolean)

    /**
     * 延迟复查：等 [LATE_RECHECK_MS] 再看一眼。
     *
     * 为什么要等：站点页面若还在 WebView 里跑着，它的 JS（心跳/定时刷新）可能在
     * 我们写完的瞬间把 cookie 又写回去 —— 现象就是"每次写入都成功、复核却永远
     * 还剩同一枚"。隔一会儿再查能区分「没清掉」与「被页面写回来了」；如果确认
     * 还在，再补一轮写入（这一次带上全部 domain 变体）。
     */
    private fun lateCheck(
        manager: CookieManager,
        hosts: List<String>,
        targets: Set<String>,
        attempts: Int,
        submit: (List<Quad>, () -> Unit) -> Unit,
        callback: ValueCallback<String>,
    ) {
        val handler = Handler(Looper.getMainLooper())
        handler.postDelayed({
            try {
                val late = targets.intersect(hosts.flatMap { probeNames(manager, it) }.toSet())
                if (late.isEmpty()) {
                    clearWebStorageThen(hosts) { bytes ->
                        finish(manager, hosts, targets, attempts, callback, rewrote = false, storageAfter = bytes)
                    }
                    return@postDelayed
                }
                // 还在：补一轮（可能是被页面写回的，也可能第一次没匹配上）。
                val domains = domainCandidatesFor(hosts.first())
                val retry = ArrayList<Quad>()
                for (h in hosts) {
                    for (n in late) {
                        for (d in domains) {
                            for (sec in SECURE_VARIANTS) retry.add(Quad(h, n, d, sec))
                        }
                    }
                }
                submit(retry) {
                    try { manager.flush() } catch (_: Throwable) { /* 不致命 */ }
                    handler.postDelayed({
                        clearWebStorageThen(hosts) { bytes ->
                            finish(manager, hosts, targets, attempts, callback, rewrote = true, storageAfter = bytes)
                        }
                    }, SETTLE_MS)
                }
            } catch (t: Throwable) {
                callback.onReceiveValue(errorJson("clearSite.lateCheck", t))
            }
        }, LATE_RECHECK_MS)
    }

    /**
     * 清掉这些源的 **web storage**（localStorage / IndexedDB），并回报残留字节数。
     *
     * 为什么必须有这一步：现代站点（Trae / WorkBuddy / qoder 这类 SPA）把登录
     * token 放在 localStorage 或 IndexedDB 里，cookie 一枚不剩也照样是登录态 ——
     * 1.0.28 真机就是这样：面板显示"已清除"，打开网站仍是旧账号。
     *
     * WebStorage 没有"按源清"之外更好的接口，但 deleteOrigin 正好是按源清。
     * origin 形态是 scheme://host（不带路径与尾斜杠）。
     *
     * 复核用 getUsageForOrigin：回调给出该源残留字节数，非 0 就是没清干净。
     * 该回调在某些版本可能不回调，所以另挂 800ms 兜底，不能卡在等待里
     * （Rust 侧最多等 15s，超时会被报成"清除超时"，比"没清掉"更难懂）。
     */
    private fun clearWebStorageThen(hosts: List<String>, onDone: (Long) -> Unit) {
        val storage = WebStorage.getInstance()
        for (h in hosts) {
            for (scheme in arrayOf("https", "http")) {
                try {
                    storage.deleteOrigin("$scheme://$h")
                } catch (_: Throwable) {
                    // 单个源失败不影响其它源，也不该让整条命令失败。
                }
            }
        }
        val handler = Handler(Looper.getMainLooper())
        var reported = false
        fun report(bytes: Long) {
            if (reported) return
            reported = true
            onDone(bytes)
        }
        // 兜底：800ms 后无论回调有没有来都收尾（读不到就报 -1，不谎称已清）。
        handler.postDelayed({ report(-1L) }, 800)
        try {
            storage.getUsageForOrigin("https://${hosts.first()}") { bytes -> report(bytes ?: 0L) }
        } catch (_: Throwable) {
            report(-1L)
        }
    }

    /** 复核：重扫，算出真正消失的与仍然存在的，如实上报。 */
    private fun finish(
        manager: CookieManager,
        hosts: List<String>,
        targets: Set<String>,
        attempts: Int,
        callback: ValueCallback<String>,
        rewrote: Boolean = false,
        storageAfter: Long = -1L,
    ) {
        try {
            val after = hosts.flatMap { probeNames(manager, it) }.toSet()
            val remaining = targets.intersect(after)
            val cleared = targets.size - remaining.size
            val names = remaining.joinToString(",") { "\"" + jsonEscape(it) + "\"" }
            callback.onReceiveValue(
                "{\"attempts\":$attempts,\"targeted\":${targets.size}," +
                    "\"cleared\":$cleared,\"remaining\":${remaining.size}," +
                    "\"remainingNames\":[$names],\"rewrote\":$rewrote,\"storageAfter\":$storageAfter}",
            )
        } catch (t: Throwable) {
            callback.onReceiveValue(errorJson("clearSite.verify", t))
        }
    }

    /**
     * 轮询等所有写回调结束（最多 2s），**不阻塞主线程** —— 用 Handler 排队。
     * 在已经就是主线程的情况下 block 等回调会直接死锁。
     */
    private fun awaitWritesThen(pending: AtomicInteger, action: () -> Unit) {
        val handler = Handler(Looper.getMainLooper())
        var waited = 0
        val tick = object : Runnable {
            override fun run() {
                if (pending.get() <= 0 || waited >= 2000) {
                    action()
                    return
                }
                waited += 20
                handler.postDelayed(this, 20)
            }
        }
        handler.postDelayed(tick, 20)
    }

    /** 探测用主机：站点本身 + 它的 apex（去 www.）—— 两处都可能有 cookie。 */
    private fun probeHostsFor(site: String): List<String> {
        val h = site.trim().removePrefix(".").lowercase()
        if (h.isEmpty()) return emptyList()
        val out = linkedSetOf(h)
        if (h.startsWith("www.")) out.add(h.removePrefix("www.")) else out.add("www.$h")
        return out.toList()
    }

    /**
     * 可能的 domain 属性候选：host-only（null）+ 各级祖先域。
     * cookie 存的是 Domain=.huaweicloud.com 时，只有用 Domain=huaweicloud.com
     * 去写才能覆盖它；反过来用 Domain=子域 写不到父域上的那份。
     */
    private fun domainCandidatesFor(site: String): List<String?> {
        val h = site.trim().removePrefix(".").lowercase()
        val parts = h.split('.').filter { it.isNotEmpty() }
        val out = ArrayList<String?>()
        out.add(null) // host-only
        // 从完整 host 一路收到剩两段（TLD+1），最多 3 个，避免写太多。
        for (i in 0..minOf(parts.size - 2, 2)) {
            val d = parts.subList(i, parts.size).joinToString(".")
            if (d.isEmpty()) continue
            out.add(d)
            // 再补一条带前导点的形式。Chrome 会把它归一化成不带点，但历史上
            // 有些实现按字面比较，多写一条成本几乎为零。
            out.add(".$d")
        }
        return out
    }

    /** 该 host 上现存（且非保护）的 cookie 名。 */
    private fun probeNames(manager: CookieManager, host: String): List<String> {
        val raw = try {
            manager.getCookie("https://$host/")
        } catch (_: Throwable) {
            null
        } ?: return emptyList()
        if (raw.isEmpty()) return emptyList()
        return raw.split(";").mapNotNull { part ->
            part.trim().substringBefore('=').takeIf { it.isNotEmpty() && !isProtected(it) }
        }.distinct()
    }

    private fun runOnMain(block: () -> Unit) {
        if (Looper.myLooper() == Looper.getMainLooper()) block()
        else Handler(Looper.getMainLooper()).post(block)
    }

    /** secure 与非 secure 的同名 cookie 在 store 里是**两条**，都得覆盖。 */
    private val SECURE_VARIANTS = booleanArrayOf(false, true)

    /** 写完后隔多久复查（够慢的 JS 定时器暴露，又不至于让用户干等）。 */
    private const val LATE_RECHECK_MS = 900L

    /** 第二轮写完后再等一下，给 store 落盘与页面回写留出窗口。 */
    private const val SETTLE_MS = 300L

    /** 把 Throwable 变成 {"error":"..."}，任何异常都不许跨 JNI 抛。 */
    internal fun errorJson(op: String, t: Throwable): String {
        val where = t.stackTrace.firstOrNull()?.let { "${it.fileName}:${it.lineNumber}" } ?: "?"
        val msg = "${t.javaClass.name}: ${t.message ?: "(no message)"} @ $where"
        return "{\"error\":${quote("$op 失败 — $msg")}}"
    }

    internal fun quote(s: String): String = buildString(s.length + 2) {
        append('"')
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
        append('"')
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
