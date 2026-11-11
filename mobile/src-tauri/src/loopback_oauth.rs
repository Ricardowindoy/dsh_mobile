// RFC 8252 loopback OAuth 回调的原生侧重写。
//
// 背景：不少登录流程（DSH 账号登录、CLI/MCP 的 OAuth）遵循 RFC 8252 —— 授权页
// 完成后 302 到 `http://127.0.0.1:<port>/oauth/callback?code=...&state=...`，
// 由发起登录的那台机器上的本地服务接住。DSH 的 `/oauth/callback` 就注册在
// webserver（默认 127.0.0.1:3082）的路由表上，并不是另起临时监听端口。
// 本机浏览器访问时一切正常；但 App 的 WebView 被导航到了公网 relay 域名，浏览器
// 跑在**手机上**，此时 127.0.0.1 指向手机自身的环回口，回调必然
// ERR_CONNECTION_REFUSED。生成的 RustWebViewClient.kt 里那段关于 oauth flow 的
// 注释说的正是这个现象。
//
// 做法：这里的导航钩子把 loopback 回调 URL 重写成 relay 上的
// `/__dsh_loopback__/<port>/oauth/callback?...`。relay 是字节管道，会把路径
// 原样透传给设备；本机 dsh-tunnel（开了 --allow-loopback-ports）识别该前缀，
// 拨 DSH 服务器的 127.0.0.1:<port> 并剥掉前缀还原路径。于是回调就落到了真正
// 发起登录的那台机器上。
//
// 为什么放在原生导航钩子而不是页面 JS：
//   · 回调是 302 顶层导航（跨域），页面脚本看不到、也拦不住；
//   · shouldInterceptRequest 在重定向时不触发，只有 shouldOverrideUrlLoading 会；
//   · wry Android 未实现 onCreateWindow，window.open('_blank') 的新窗口导航
//     会重定向回同一个 WebView，同样经过这个钩子。
//
// relay 地址来源：不能用 webview.url()（回调发生时当前页面是外部授权页），
// 必须读 App 持久化的 config.json —— 那是连接页保存下来的 relay 入口。
// 回调是一次性事件（非热路径），每次读一次盘换取「不需要在 save_config
// 与 setup 之间维护共享缓存」的简单性。
//
// 安全：只重写 **HTTP + loopback 主机名 + 显式端口** 的 URL；其它一切放行。
// 端口由 URL 给出，dial 目标恒为 127.0.0.1（tunnel 侧强制），手机无法指定主机。

use serde_json::Value;
use std::sync::OnceLock;
use tauri::{
    plugin::{Builder as PluginBuilder, TauriPlugin},
    Manager, Runtime, Url, Webview,
};

/// relay 侧的 loopback 回调前缀，必须与
/// `internal/tunnel/tunnel.go:loopbackPathPrefix` 保持一致。
const LOOPBACK_PREFIX: &str = "/__dsh_loopback__/";

/// relay 入口缓存。on_navigation 在 Android 上跑在 WebView 的同步
/// shouldOverrideUrlLoading 链路里，任何文件 IO 都可能拖住主线程（实测 5s
/// 就触发 ANR），所以这里只在启动时读一次盘，之后走内存。
static RELAY_ORIGIN: OnceLock<Option<String>> = OnceLock::new();

/// 构造插件。用 plugin.on_navigation 而不是 WebviewWindow::on_navigation，
/// 因为前者在 Android 的 shouldOverrideUrlLoading 链路上会被调用。
pub fn loopback_oauth_plugin<R: Runtime>() -> TauriPlugin<R> {
    PluginBuilder::new("dsh-loopback-oauth")
        .setup(|app, _| {
            // 预热：把 relay 入口在启动线程读一次，导航回调里只碰内存。
            let warm = relay_origin(app);
            eprintln!("[dsh-loopback-oauth] setup warm-up ok={}", warm.is_some());
            Ok(())
        })
        .on_navigation(|webview: &Webview<R>, url: &Url| {
            let Some(relay_raw) = relay_origin(webview.app_handle()) else {
                return true; // 拿不到 relay 入口：放行，退化为原来的失败行为
            };
            let Ok(relay) = Url::parse(&relay_raw) else {
                return true;
            };
            let Some(rewritten) = rewrite_loopback_callback(&relay, url) else {
                return true; // 非 loopback 回调：放行
            };
            // 返回 false = 取消这次导航；改导航到 relay 路径。
            // webview.navigate() 在 Android 上是 post 到主线程（loadUrlMainThread），
            // 不会与同步的 shouldOverride 回调嵌套成递归；且重写后的 URL 不再是
            // loopback，二次进入本钩子时会直接放行。
            if let Err(e) = webview.navigate(rewritten) {
                eprintln!("[dsh-loopback-oauth] navigate failed: {e}");
            }
            false
        })
        .build()
}

/// 取缓存中的 relay 入口；未预热时读一次盘并缓存（仅在 setup 之外调用时发生）。
fn relay_origin<R: Runtime>(app: &tauri::AppHandle<R>) -> Option<String> {
    RELAY_ORIGIN
        .get_or_init(|| read_relay_url(app))
        .clone()
}

/// 读 App 持久化的 config.json，取 relayUrl 字段。
fn read_relay_url<R: Runtime>(app: &tauri::AppHandle<R>) -> Option<String> {
    let dir = app.path().app_config_dir().ok()?;
    let data = std::fs::read(dir.join("config.json")).ok()?;
    let cfg: Value = serde_json::from_slice(&data).ok()?;
    let raw = cfg.get("relayUrl")?.as_str()?.trim();
    if raw.is_empty() {
        return None;
    }
    let url = Url::parse(raw).ok()?;
    if !matches!(url.scheme(), "http" | "https") {
        return None;
    }
    // 归一化成不带 path/query 的 origin + 保留可能的 /d/<id> 前缀。
    let mut out = url.clone();
    out.set_query(None);
    out.set_fragment(None);
    Some(out.to_string())
}

/// 把 `http://127.0.0.1:<port>/<path>?<query>` 重写成
/// `<relay>/__dsh_loopback__/<port>/<path>?<query>`。
/// 返回 None 表示这不是需要处理的 loopback 回调（调用方应放行）。
fn rewrite_loopback_callback(relay: &Url, url: &Url) -> Option<Url> {
    // 只处理 HTTP + loopback + 显式端口，且 relay 本身必须是 http(s)。
    if url.scheme() != "http" || !matches!(relay.scheme(), "http" | "https") {
        return None;
    }
    let host = url.host_str()?;
    if !matches!(host, "127.0.0.1" | "localhost" | "[::1]" | "::1") {
        return None;
    }
    let port = url.port()?; // RFC 8252 要求显式端口；无端口一律不处理

    // relay 当前路径可能是 /d/<deviceId>/（多设备入口）。只保留这一层路由
    // 前缀，不能把 SPA 的深层路径也拼进去；relay.pickDevice 靠它找设备。
    let relay_path = relay.path().trim_end_matches('/');
    let base = format!("{}://{}", relay.scheme(), relay.authority());
    let prefix = if relay_path.is_empty() || relay_path == "/" {
        String::new()
    } else {
        relay_path.to_string()
    };

    let path = url.path().trim_start_matches('/');
    let new_path = format!("{}{}{}/{}", prefix, LOOPBACK_PREFIX, port, path);
    // 用 parse 拼出完整 URL，再以原 URL 的 query 原样覆盖，保证 code/state 的
    // 原始编码一字不改地透传给回调校验（dsh 的 state 是 timingSafeEqual 比对的）。
    let mut parsed = Url::parse(&format!("{}{}", base, new_path)).ok()?;
    parsed.set_query(url.query());
    Some(parsed)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn relay() -> Url {
        Url::parse("https://dsh.example.com:24000/").unwrap()
    }

    #[test]
    fn rewrites_loopback_callback_with_query() {
        let url = Url::parse("http://127.0.0.1:3082/oauth/callback?code=a%2Bb&state=s%3D1").unwrap();
        let got = rewrite_loopback_callback(&relay(), &url).unwrap();
        assert_eq!(got.scheme(), "https");
        assert_eq!(got.host_str(), Some("dsh.example.com"));
        assert_eq!(got.port(), Some(24000));
        assert_eq!(got.path(), "/__dsh_loopback__/3082/oauth/callback");
        // query 原始编码必须保留（code 里有 %2B，不能被二次编码或解码）
        assert_eq!(got.query(), Some("code=a%2Bb&state=s%3D1"));
    }

    #[test]
    fn rewrites_localhost_and_root_callback() {
        let url = Url::parse("http://localhost:8976/cb?code=x").unwrap();
        let got = rewrite_loopback_callback(&relay(), &url).unwrap();
        assert_eq!(got.path(), "/__dsh_loopback__/8976/cb");
        assert_eq!(got.query(), Some("code=x"));

        let root = Url::parse("http://[::1]:49152/").unwrap();
        let got2 = rewrite_loopback_callback(&relay(), &root).unwrap();
        assert_eq!(got2.path(), "/__dsh_loopback__/49152/");
    }

    #[test]
    fn preserves_device_routing_prefix() {
        let relay = Url::parse("https://dsh.example.com:24000/d/home-dsh/").unwrap();
        let url = Url::parse("http://127.0.0.1:3082/oauth/callback?code=x").unwrap();
        let got = rewrite_loopback_callback(&relay, &url).unwrap();
        assert_eq!(got.path(), "/d/home-dsh/__dsh_loopback__/3082/oauth/callback");
    }

    #[test]
    fn ignores_non_loopback() {
        for raw in [
            "https://dsh.example.com/api/x",
            "http://192.168.1.5:3082/oauth/callback",
            "https://127.0.0.1:3082/oauth/callback", // 只处理 http
            "http://127.0.0.1/oauth/callback",        // 无显式端口
            "http://evil.example.com:3082/cb",
        ] {
            let url = Url::parse(raw).unwrap();
            assert!(
                rewrite_loopback_callback(&relay(), &url).is_none(),
                "should not rewrite {raw}"
            );
        }
    }

    #[test]
    fn ignores_invalid_relay_scheme() {
        let url = Url::parse("http://127.0.0.1:3082/cb?code=x").unwrap();
        let bad = Url::parse("tauri://localhost/").unwrap();
        assert!(rewrite_loopback_callback(&bad, &url).is_none());
    }
}
