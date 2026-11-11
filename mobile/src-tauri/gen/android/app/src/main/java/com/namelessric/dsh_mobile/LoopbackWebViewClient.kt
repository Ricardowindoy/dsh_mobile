package com.namelessric.dsh_mobile

import android.graphics.Bitmap
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.webkit.WebViewCompat

/**
 * 在 Tauri/wry 安装好的 [RustWebViewClient] 外面套一层纯 Kotlin 的
 * `shouldOverrideUrlLoading`，只做 RFC 8252 loopback 回调改写，其余全部
 * 原样委托给原 client（asset loader、Rust IPC、错误重试都不变）。
 *
 * 为什么不改 `generated/RustWebViewClient.kt`：那个文件每次
 * `tauri android init/build` 都可能重新生成，且被 .gitignore 排除；把逻辑
 * 放在这个受版本控制的文件里，升级 Tauri 后不会丢。
 *
 * 为什么用委托而不是继承：wry 生成的 `RustWebViewClient` 是 final class。
 */
internal class LoopbackWebViewClient(
    private val delegate: WebViewClient,
) : WebViewClient() {
    override fun shouldInterceptRequest(
        view: WebView,
        request: WebResourceRequest,
    ): WebResourceResponse? = delegate.shouldInterceptRequest(view, request)

    override fun shouldOverrideUrlLoading(
        view: WebView,
        request: WebResourceRequest,
    ): Boolean {
        // 回调路径只做内存判断；warm() 仅在缓存为空时启动一次后台读盘线程。
        LoopbackOAuthRelay.warm(view.context)
        LoopbackOAuthRelay.rewrite(request.url)?.let { rewritten ->
            view.loadUrl(rewritten)
            return true
        }
        return delegate.shouldOverrideUrlLoading(view, request)
    }

    override fun onPageStarted(view: WebView, url: String, favicon: Bitmap?) {
        LoopbackOAuthRelay.warm(view.context)
        delegate.onPageStarted(view, url, favicon)
    }

    override fun onPageFinished(view: WebView, url: String) {
        delegate.onPageFinished(view, url)
    }

    override fun onReceivedError(
        view: WebView,
        request: WebResourceRequest,
        error: WebResourceError,
    ) {
        delegate.onReceivedError(view, request, error)
    }
}

/**
 * 把 wry 装好的 client 包一层；已包过或 client 尚未就绪时返回 false。
 *
 * `WebViewCompat.getWebViewClient` 在 minSdk 24 上可用（androidx.webkit 1.14），
 * 避免直接用 API 26 才有的 `WebView.getWebViewClient()`。
 */
internal fun wrapWebViewClientForLoopback(webView: WebView): Boolean {
    val current = WebViewCompat.getWebViewClient(webView) ?: return false
    if (current is LoopbackWebViewClient) return true
    webView.webViewClient = LoopbackWebViewClient(current)
    return true
}

/**
 * 等 wry 的 `setWebViewClient` 落地后再包（JNI 里 setWebView 在前）。
 * 首次 0ms，之后每 100ms 重试，最多 20 次（约 2s）。
 */
internal fun installLoopbackWebViewClient(webView: WebView, attempts: Int = 20) {
    webView.postDelayed({
        if (!wrapWebViewClientForLoopback(webView) && attempts > 0) {
            installLoopbackWebViewClient(webView, attempts - 1)
        }
    }, if (attempts == 20) 0L else 100L)
}
