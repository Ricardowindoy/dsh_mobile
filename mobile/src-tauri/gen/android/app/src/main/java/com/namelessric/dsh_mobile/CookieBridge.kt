package com.namelessric.dsh_mobile

import android.os.Handler
import android.os.Looper
import android.webkit.CookieManager
import android.webkit.ValueCallback

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
 * [LOGIN_HOSTS] 逐个取自 JetHub 插件里**真实会打开**的登录地址
 * （JetHub 的 lib 目录下各 JS 与 plugin-client/jet-hub.js 里带
 * login/auth/oauth/sso 的 URL）。
 *
 * ⚠️ 别在这段 KDoc 里写形如 lib/＊.js 的字面量：Kotlin 块注释**允许嵌套**，
 * 文本里出现斜杠加星号就会开一个永不闭合的嵌套注释，整个文件被吞掉，
 * 报错只有一句 `Unclosed comment`（1.0.24 首次 CI 就这样红的）。
 * 清单只用于「探测」，不用于「展示」：只有真查到 cookie 的域才进列表 ——
 * 列一个空站点等于撒谎。
 *
 * ## API 约束
 *
 * - [CookieManager.getCookie] 只能按 URL 查，不能枚举全 store → 必须靠清单探测。
 * - 回调与 setCookie 必须回主线程（WebView 硬约束），统一走 [runOnMain]。
 * - 删除靠「写一条 Max-Age=0 的同名 cookie 覆盖」：CookieManager 没有按域批量删
 *   的 API，removeAllCookies 是全清（不能用）。
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
     * 回调在主线程，形如 [{site, cookieCount, names}]。
     */
    @JvmStatic
    fun sites(callback: ValueCallback<String>) {
        runOnMain {
            val manager = CookieManager.getInstance()
            val out = StringBuilder("[")
            var first = true
            for (host in LOGIN_HOSTS) {
                val names = cookieNamesFor(manager, host)
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
        }
    }

    /**
     * 清掉 [site]（及其 www / 前后点变体）的 cookie。
     *
     * @param cookieNames 要清的具体 cookie 名；空 = 该域下全部。
     * @param callback 回调 {"requested":n,"cleared":n}
     */
    @JvmStatic
    fun clearSite(site: String, cookieNames: Array<String>, callback: ValueCallback<String>) {
        runOnMain {
            val manager = CookieManager.getInstance()
            var requested = 0
            var cleared = 0
            // 同一份 cookie 可能以 apex / 带点 / www 三种 domain 属性存在，
            // 逐个写过期覆盖，才不会有漏网的。
            val variants = linkedSetOf(site, ".$site", "www.$site", ".www.$site")
            for (host in variants) {
                val names = cookieNamesFor(manager, host)
                for (n in names) {
                    if (isProtected(n)) continue
                    if (cookieNames.isNotEmpty() && !cookieNames.contains(n)) continue
                    requested++
                    if (expire(manager, host, n)) cleared++
                }
            }
            // 不调 removeExpiredCookies()：它在 API 33 起废弃、在 android-36 的
            // android.jar 里已经**不存在**（javap 实测只剩单数 removeExpiredCookie）。
            // 这里也不需要它——过期 cookie 由 WebView 自动淘汰，flush() 负责落盘。
            manager.flush()
            callback.onReceiveValue("{\"requested\":$requested,\"cleared\":$cleared}")
        }
    }

    /** 该 host 上现存（且非保护）的 cookie 名。 */
    private fun cookieNamesFor(manager: CookieManager, host: String): List<String> {
        val raw = try {
            manager.getCookie("https://$host/")
        } catch (_: Throwable) {
            return emptyList()
        } ?: return emptyList()
        if (raw.isEmpty()) return emptyList()
        return raw.split(";").mapNotNull { part ->
            part.trim().substringBefore('=').takeIf { it.isNotEmpty() && !isProtected(it) }
        }
    }

    /** 用 Max-Age=0 覆盖同名 cookie；成功返回 true。 */
    private fun expire(manager: CookieManager, host: String, name: String): Boolean {
        val domain = host.removePrefix(".")
        val expired = "$name=; Domain=$domain; Path=/; Max-Age=0; " +
            "Expires=Thu, 01 Jan 1970 00:00:00 GMT"
        return try {
            manager.setCookie("https://$host/", expired)
            true
        } catch (_: Throwable) {
            false
        }
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
