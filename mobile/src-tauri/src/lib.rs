// dsh-mobile: a thin native shell around the dsh Web GUI. The bundled
// frontend collects the relay URL + access token, then navigates the
// WebView to the relay; everything else is plain web content.
//
// Config (relayUrl/accessToken) is persisted to a JSON file in the app
// config dir from the Rust side — WebView localStorage alone is not
// guaranteed to survive app restarts on Android.
//
// 应用内自动更新：download_update 把 APK 流式下载到应用缓存目录并校验
// SHA-256；install_update 通过 JNI 构造 FileProvider content:// URI +
// ACTION_VIEW 拉起系统安装器（Android WebView/opener 插件都无法直接装
// 本地 APK，必须原生侧发 intent）。

// loopback OAuth 回调功能：插件已保留但**暂不注册**。
// 0.1.16/0.1.17 的 on_navigation 钩子在 Android 上导致主线程 ANR → 白屏，
// 定位见 docs/loopback-white-screen-postmortem.md。重做前不要打开。
#[allow(dead_code)]
mod loopback_oauth;

// 审批/提问推送（Android only）：Rust SSE 订阅 + JNI 通知卡片。
#[cfg(target_os = "android")]
mod notify;

/// 点通知要跳转的会话 id。Kotlin 在主线程把它塞进来，setup 页导航前用
/// `take_pending_session` 取走（取走即清空，避免下次启动重复跳转）。
static PENDING_SESSION: Mutex<Option<String>> = Mutex::new(None);

/// 取走待跳转的会话 id（读取即清空）。
#[tauri::command]
fn take_pending_session() -> Option<String> {
    PENDING_SESSION.lock().ok().and_then(|mut guard| guard.take())
}

/// Kotlin 的 `MainActivity.nativeSetPendingSession`（JNI）：记录待跳转会话。
#[cfg(target_os = "android")]
#[no_mangle]
pub extern "system" fn Java_com_namelessric_dsh_1mobile_MainActivity_nativeSetPendingSession(
    mut env: jni::JNIEnv,
    _class: jni::objects::JClass,
    session: jni::objects::JString,
) {
    let Ok(sid) = env.get_string(&session).map(|s| s.to_string_lossy().into_owned()) else { return };
    if sid.is_empty() {
        return;
    }
    if let Ok(mut guard) = PENDING_SESSION.lock() {
        *guard = Some(sid);
    }
}

use serde_json::{json, Value};
use std::sync::Mutex;
use std::time::Instant;
use tauri::Manager;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    // 记录 Rust 侧启动起点，用于测量"进程/WebView 冷启动"到 JS 就绪的耗时
    // （这段是 JS 侧 boot-timing 打点无法覆盖的，是排查连接慢的关键缺口）。
    let started = Instant::now();
    let builder = tauri::Builder::default()
        .plugin(tauri_plugin_opener::init());
    // 相机扫码已改为 WebView getUserMedia + jsQR（main.js），不再依赖
    // barcode-scanner 插件（其 MLKit/CameraX native 路径在目标设备上闪退）。
    #[cfg(not(target_os = "android"))]
    let builder = builder.invoke_handler(tauri::generate_handler![
        save_config,
        load_config,
        boot_elapsed_ms,
        download_update,
        install_update,
        set_status_theme,
        pair_redeem,
        pair_refresh,
        read_clipboard,
        read_crash_log,
        take_pending_session
    ]);
    #[cfg(target_os = "android")]
    let builder = builder.invoke_handler(tauri::generate_handler![
        save_config,
        load_config,
        boot_elapsed_ms,
        download_update,
        install_update,
        set_status_theme,
        pair_redeem,
        pair_refresh,
        read_clipboard,
        read_crash_log,
        notify::notify_start,
        notify::notify_stop,
        take_pending_session
    ]);
    builder
        // loopback 插件暂不注册（白屏根因，见 loopback_oauth.rs 顶部说明）。
        // .plugin(loopback_oauth::loopback_oauth_plugin())
        .setup(move |app| {
            app.manage(started);
            // Rust panic 落盘：命令处理线程 panic 会直接杀进程（用户看到"闪退"，
            // 且 JS 来不及写任何日志）。把 panic 信息追加到 app log 目录，
            // 下次启动由 read_crash_log 取走并进入 App 日志，随"推送日志"上传。
            {
                use std::io::Write;
                if let Ok(dir) = app
                    .path()
                    .app_log_dir()
                    .or_else(|_| app.path().app_cache_dir())
                {
                    let _ = std::fs::create_dir_all(&dir);
                    let path = dir.join("rust-panic.log");
                    let default_hook = std::panic::take_hook();
                    std::panic::set_hook(Box::new(move |info| {
                        if let Ok(mut f) = std::fs::OpenOptions::new()
                            .create(true)
                            .append(true)
                            .open(&path)
                        {
                            let _ = writeln!(f, "[{}] {}", std::process::id(), info);
                        }
                        default_hook(info);
                    }));
                }
            }
            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running dsh-mobile");
}

/// 返回自 Rust `run()` 启动以来经过的毫秒数，供 JS 在 boot 起点打点，
/// 从而把"App 进程/WebView 冷启动"也纳入 boot-timing 时间线。
#[tauri::command]
fn boot_elapsed_ms(app: tauri::AppHandle) -> u64 {
    let started = app.state::<Instant>();
    started.elapsed().as_millis() as u64
}

fn config_path(app: &tauri::AppHandle) -> Result<std::path::PathBuf, String> {
    let dir = app
        .path()
        .app_config_dir()
        .map_err(|e| format!("app_config_dir: {e}"))?;
    std::fs::create_dir_all(&dir).map_err(|e| format!("create config dir: {e}"))?;
    Ok(dir.join("config.json"))
}

/// 持久化保存 App 配置（relayUrl/accessToken）。
#[tauri::command]
fn save_config(app: tauri::AppHandle, config: Value) -> Result<(), String> {
    let path = config_path(&app)?;
    let data = serde_json::to_vec(&config).map_err(|e| format!("encode: {e}"))?;
    std::fs::write(&path, data).map_err(|e| format!("write {}: {e}", path.display()))
}

/// 读取持久化配置；不存在时返回 null。
#[tauri::command]
fn load_config(app: tauri::AppHandle) -> Result<Option<Value>, String> {
    let path = config_path(&app)?;
    match std::fs::read(&path) {
        Ok(data) => serde_json::from_slice(&data).map(Some).map_err(|e| format!("decode: {e}")),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(format!("read {}: {e}", path.display())),
    }
}

// ---------------------------------------------------------------------------
// 自动更新
// ---------------------------------------------------------------------------

/// 私有 CA（自签）：relay 现在以「公网 IP + 私有 CA」提供 TLS，而 reqwest 的
/// rustls 走 webpki-roots（**不读 Android 信任库**，NSC 对它无效），因此必须
/// 显式注入这份 CA，否则更新检查/下载在 https 下必然报证书错误。
/// 与 APK 内置的 NSC 锚点同源（res/raw/dsh_relay_ca.crt），两份必须同步更新。
const RELAY_CA_PEM: &[u8] = include_bytes!("../certs/dsh-relay-ca.crt");

/// 进度事件的载荷（经 tauri::ipc::Channel 序列化为 JSON 推给 WebView）。
/// 服务器没回 Content-Length 时 total 为 null → 前端显示不定态进度。
#[derive(Clone, serde::Serialize)]
struct DownloadProgress {
    received: u64,
    total: Option<u64>,
}

/// 把 APK 流式下载到应用缓存目录 updates/update.apk，边下边算 SHA-256；
/// expected_sha256 非空时校验不匹配即报错并删除半成品。返回 {path,size}。
/// on_progress：必填的 tauri::ipc::Channel（tauri 2.12.5 没给 Option<Channel>
/// 的 CommandArg impl——Channel 只实现 Serialize），每个写入块都推
/// {received,total}；首尾事件保证「开始」与「100%」总会出现，不依赖分块边界。
#[tauri::command]
async fn download_update(
    app: tauri::AppHandle,
    on_progress: tauri::ipc::Channel<DownloadProgress>,
    url: String,
    expected_sha256: Option<String>,
) -> Result<Value, String> {
    use sha2::Digest;
    use tokio::io::AsyncWriteExt;

    let root = reqwest::Certificate::from_pem(RELAY_CA_PEM)
        .map_err(|e| format!("内置 relay CA 解析失败: {e}"))?;
    let client = reqwest::Client::builder()
        .add_root_certificate(root)
        .connect_timeout(std::time::Duration::from_secs(15))
        .read_timeout(std::time::Duration::from_secs(30))
        .build()
        .map_err(|e| format!("构建 HTTP 客户端失败: {e}"))?;
    let mut resp = client
        .get(&url)
        .send()
        .await
        .map_err(|e| format!("连接更新服务器失败: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("更新服务器响应异常（HTTP {}）", resp.status()));
    }
    // Content-Length 有无决定前端进度是「定态百分比」还是「不定态」；
    // 有值也只作展示上界，真实进度以收到的字节为准（收到超量前端自行钳 100）。
    let total = resp
        .content_length()
        .filter(|&t| t > 0);

    let dir = app
        .path()
        .app_cache_dir()
        .map_err(|e| format!("cache dir: {e}"))?
        .join("updates");
    tokio::fs::create_dir_all(&dir)
        .await
        .map_err(|e| format!("create update dir: {e}"))?;
    let tmp_path = dir.join("update.apk.part");
    let final_path = dir.join("update.apk");

    let mut hasher = sha2::Sha256::new();
    let mut file = tokio::fs::File::create(&tmp_path)
        .await
        .map_err(|e| format!("create temp file: {e}"))?;
    let mut size: u64 = 0;
    // 首个进度事件：0 字节也立即出现（不定态时这是唯一能看到"活着"的事件）
    let _ = on_progress.send(DownloadProgress { received: 0, total });
    while let Some(chunk) = resp
        .chunk()
        .await
        .map_err(|e| format!("下载中断: {e}"))?
    {
        hasher.update(&chunk);
        file.write_all(&chunk)
            .await
            .map_err(|e| format!("写入临时文件失败: {e}"))?;
        size += chunk.len() as u64;
        // send 失败只意味着 WebView 已关闭/跳走：下载继续，不因进度上报失败而中断
        let _ = on_progress.send(DownloadProgress {
            received: size,
            total,
        });
    }
    file.flush().await.ok();
    drop(file);

    let digest: String = hasher.finalize().iter().map(|b| format!("{b:02x}")).collect();
    if let Some(expected) = expected_sha256.as_deref().map(str::trim).filter(|s| !s.is_empty()) {
        if !digest.eq_ignore_ascii_case(expected) {
            let _ = tokio::fs::remove_file(&tmp_path).await;
            return Err("更新包完整性校验失败（SHA-256 不匹配），已放弃安装".into());
        }
    }
    tokio::fs::rename(&tmp_path, &final_path)
        .await
        .map_err(|e| format!("finalize update file: {e}"))?;

    Ok(json!({
        "path": final_path.to_string_lossy(),
        "size": size,
        "sha256": digest,
    }))
}

// --- 扫码配对 ---------------------------------------------------------------
//
// 长效设备 token 只在 Rust↔relay 的 HTTPS body/header 里流动，绝不进 WebView
// URL。二维码里只有一次性 code；redeem 用 POST body 交 code，拿回
// {deviceToken, ticket}；WebView 只导航 login?ticket=<一次性60s>。
// 域名入口走公共 LE 链（webpki-roots），IP 入口显式注入内置私有 CA——
// reqwest/rustls 不读 Android 信任库（与 download_update 同一个坑）。

fn relay_client(relay_url: &str) -> Result<reqwest::Client, String> {
    let host = reqwest::Url::parse(relay_url)
        .ok()
        .and_then(|u| u.host_str().map(str::to_string))
        .unwrap_or_default();
    let is_ip = host.parse::<std::net::IpAddr>().is_ok();
    let mut builder = reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(15))
        .read_timeout(std::time::Duration::from_secs(30));
    if is_ip {
        let root = reqwest::Certificate::from_pem(RELAY_CA_PEM)
            .map_err(|e| format!("内置 relay CA 解析失败: {e}"))?;
        builder = builder.add_root_certificate(root);
    }
    builder.build().map_err(|e| format!("构建 HTTP 客户端失败: {e}"))
}

/// POST pair/redeem：一次性 code 换设备 token + 登录 ticket。
/// 返回 relay 的 JSON 原文（{deviceToken, deviceId, ticket, ticketExpiresAt}）。
#[tauri::command]
async fn pair_redeem(relay_url: String, code: String, name: String) -> Result<Value, String> {
    let base = relay_url.trim_end_matches('/');
    let client = relay_client(base)?;
    let resp = client
        .post(format!("{base}/__dsh_tunnel__/pair/redeem"))
        .json(&json!({ "code": code, "name": name }))
        .send()
        .await
        .map_err(|e| format!("连接 relay 失败: {e}"))?;
    let status = resp.status();
    // 先按字节读、成功才 parse：错误路径 relay 用 http.Error 写 text/plain，
    // 直接 resp.json() 会把 404 的 "pair code expired or already used"
    // 吞成 "relay 响应解析失败: error decoding response body"（1.0.4 实测）。
    let bytes = resp
        .bytes()
        .await
        .map_err(|e| format!("读取 relay 响应失败: {e}"))?;
    if !status.is_success() {
        let detail = String::from_utf8_lossy(&bytes);
        let detail = detail.trim();
        return Err(match status {
            reqwest::StatusCode::NOT_FOUND => "配对码已过期或已被使用，请在桌面刷新二维码".into(),
            reqwest::StatusCode::TOO_MANY_REQUESTS => "尝试次数过多，请一分钟后再试".into(),
            _ if detail.is_empty() => format!("配对失败（HTTP {status}）"),
            _ => format!("配对失败（HTTP {status}）: {detail}"),
        });
    }
    let body: Value = serde_json::from_slice(&bytes)
        .map_err(|e| format!("relay 响应解析失败: {e}"))?;
    Ok(body)
}

/// POST pair/refresh：已配对设备（header 携带设备 token）换新登录 ticket，
/// 用于 WebView cookie 被清后的免扫码重连。
#[tauri::command]
async fn pair_refresh(relay_url: String, device_token: String) -> Result<Value, String> {
    let base = relay_url.trim_end_matches('/');
    let client = relay_client(base)?;
    let resp = client
        .post(format!("{base}/__dsh_tunnel__/pair/refresh"))
        .header("X-DSH-Relay-Token", device_token)
        .send()
        .await
        .map_err(|e| format!("连接 relay 失败: {e}"))?;
    let status = resp.status();
    if status == reqwest::StatusCode::UNAUTHORIZED {
        return Err("设备凭据已被撤销，请重新扫码配对".into());
    }
    if !status.is_success() {
        return Err(format!("刷新登录失败（HTTP {status}）"));
    }
    // 同 pair_redeem：先字节后 parse，避免错误文本响应被吞成解析错误。
    let bytes = resp
        .bytes()
        .await
        .map_err(|e| format!("读取 relay 响应失败: {e}"))?;
    serde_json::from_slice(&bytes).map_err(|e| format!("relay 响应解析失败: {e}"))
}

// --- Android JNI 桥 ---------------------------------------------------------

/// MainActivity.onCreate 里通过 `external fun nativeInitBridge(activity)` 调用，
/// 把 JavaVM 与 Activity 存进静态量，供 install_update / set_status_theme 使用。
/// （Tauri AppHandle 没有公开 JNI 入口，这是最短的桥接路径。）
#[cfg(target_os = "android")]
mod android_bridge {
    use jni::objects::{GlobalRef, JObject};
    use jni::JNIEnv;
    use std::sync::OnceLock;

    pub static VM: OnceLock<jni::JavaVM> = OnceLock::new();
    pub static ACTIVITY: OnceLock<GlobalRef> = OnceLock::new();
    /// NotifyBridge 的 Class 全局引用：在 MainActivity.onCreate 的主线程里
    /// FindClass 能走应用 classloader；attached 线程里找不到应用类。
    pub static NOTIFY_CLASS: OnceLock<GlobalRef> = OnceLock::new();

    /// 包名 com.namelessric.dsh_mobile 的 JNI 段为 com/namelessric/dsh_1mobile
    /// （下划线转义成 _1），对应 Kotlin MainActivity companion 里的
    /// @JvmStatic external fun nativeInitBridge(Activity)。
    #[no_mangle]
    pub extern "system" fn Java_com_namelessric_dsh_1mobile_MainActivity_nativeInitBridge(
        mut env: JNIEnv,
        _class: jni::objects::JClass,
        activity: JObject,
    ) {
        let Ok(vm) = env.get_java_vm() else { return };
        let Ok(global) = env.new_global_ref(&activity) else { return };
        let _ = VM.set(vm);
        let _ = ACTIVITY.set(global);
        // 这里在 Java 调用的主线程上，FindClass 用 MainActivity 的
        // classloader，能拿到应用类；缓存成全局引用供 notify 线程复用。
        if let Ok(cls) = env.find_class("com/namelessric/dsh_mobile/NotifyBridge") {
            if let Ok(g) = env.new_global_ref(&cls) {
                let _ = NOTIFY_CLASS.set(g);
            }
        }
    }

    /// 主线程切换状态栏配色：decorView 背景（即安全区条带）+ 图标明暗。
    /// 构造 Kotlin StatusBarThemeTask 后 Handler(mainLooper).post 执行——
    /// 视图操作必须回主线程，而 JNI 侧 post 需要一个实体 Runnable 类。
    pub fn status_theme(bg_hex: &str, light_icons: bool) -> Result<(), String> {
        use jni::objects::JClass;
        use jni::objects::JValue;

        let vm = VM.get().ok_or("native bridge 未初始化")?;
        let act = ACTIVITY.get().ok_or("activity 未初始化")?;
        let mut env = vm.attach_current_thread().map_err(|e| format!("attach jvm: {e}"))?;
        let activity: &JObject = act.as_obj();

        let hex = bg_hex.trim().trim_start_matches('#');
        let rgb = u32::from_str_radix(hex, 16)
            .map_err(|e| format!("invalid color '{bg_hex}': {e}"))?;
        let argb = (0xFF00_0000u32 | (rgb & 0x00FF_FFFF)) as i32;

        // App 类在 PathClassLoader 里，attached 线程的 find_class 看不到，
        // 必须经 activity 的 classloader 加载。
        let cls_obj = env
            .call_method(activity, "getClass", "()Ljava/lang/Class;", &[])
            .map_err(|e| format!("getClass: {e}"))?
            .l()
            .map_err(|e| e.to_string())?;
        let loader = env
            .call_method(&cls_obj, "getClassLoader", "()Ljava/lang/ClassLoader;", &[])
            .map_err(|e| format!("getClassLoader: {e}"))?
            .l()
            .map_err(|e| e.to_string())?;
        let task_name = env
            .new_string("com/namelessric/dsh_mobile/StatusBarThemeTask")
            .map_err(|e| e.to_string())?;
        let task_cls = env
            .call_method(
                &loader,
                "loadClass",
                "(Ljava/lang/String;)Ljava/lang/Class;",
                &[(&task_name).into()],
            )
            .map_err(|e| format!("loadClass StatusBarThemeTask: {e}"))?
            .l()
            .map_err(|e| e.to_string())?;

        let task = env
            .new_object(
                JClass::from(task_cls),
                "(Landroid/app/Activity;IZ)V",
                &[
                    activity.into(),
                    JValue::Int(argb),
                    JValue::Bool(if light_icons { 1 } else { 0 }),
                ],
            )
            .map_err(|e| format!("new StatusBarThemeTask: {e}"))?;

        let looper = env
            .call_static_method("android/os/Looper", "getMainLooper", "()Landroid/os/Looper;", &[])
            .map_err(|e| format!("getMainLooper: {e}"))?
            .l()
            .map_err(|e| e.to_string())?;
        let handler = env
            .new_object("android/os/Handler", "(Landroid/os/Looper;)V", &[(&looper).into()])
            .map_err(|e| format!("new Handler: {e}"))?;
        env.call_method(&handler, "post", "(Ljava/lang/Runnable;)Z", &[(&task).into()])
            .map_err(|e| format!("Handler.post: {e}"))?;
        Ok(())
    }
}

/// 切换状态栏条带配色与图标明暗。进入浅色 GUI 前由前端调用：
/// set_status_theme({ bg: "#ffffff", lightIcons: false })。
#[cfg(target_os = "android")]
#[tauri::command]
fn set_status_theme(bg: String, light_icons: bool) -> Result<(), String> {
    android_bridge::status_theme(&bg, light_icons)
}

/// 非 Android 平台的占位实现（桌面开发预览用不到）。
#[cfg(not(target_os = "android"))]
#[tauri::command]
fn set_status_theme(_bg: String, _light_icons: bool) -> Result<(), String> {
    Ok(())
}

/// 把 JNI 挂起异常的类名 + getMessage 取出来（供 install_update 报错定位）。
/// ⚠️ jni crate 的 check_exception 宏返回 Err(JavaException) 时【不清除】异常，
/// 所以本函数必须先 exception_clear()——挂起异常未清时 JNI 会拒绝大部分
/// 对象方法调用，1.0.5 之前 getName/getMessage 全部失败只剩 "?"。
#[cfg(target_os = "android")]
fn jni_pending_exception(env: &mut jni::JNIEnv) -> Option<String> {
    let throwable = env.exception_occurred().ok()?;
    if throwable.is_null() {
        return None;
    }
    // 先清掉挂起状态，后续对象方法调用才被 JNI 允许。
    env.exception_clear().ok();
    let detail = (|| -> Option<String> {
        // Throwable.getMessage()
        let msg = env
            .call_method(&throwable, "getMessage", "()Ljava/lang/String;", &[])
            .ok()?
            .l()
            .ok()?;
        if msg.is_null() {
            return None;
        }
        let s: String = env.get_string((&msg).into()).ok()?.into();
        if s.is_empty() { None } else { Some(s) }
    })();
    let cls_name: Option<String> = (|| -> Option<String> {
        // Class.getName() —— jni crate 没有 name() 助手，直接经反射调。
        let cls = env.get_object_class(&throwable).ok()?;
        let name = env
            .call_method(cls, "getName", "()Ljava/lang/String;", &[])
            .ok()?
            .l()
            .ok()?;
        if name.is_null() {
            return None;
        }
        let s: String = env.get_string((&name).into()).ok()?.into();
        if s.is_empty() { None } else { Some(s) }
    })();
    let cls_name = cls_name.unwrap_or_else(|| "?".into());
    Some(match detail {
        Some(m) => format!("{cls_name}: {m}"),
        None => cls_name,
    })
}

/// 用 FileProvider content:// URI + ACTION_VIEW 拉起系统 APK 安装器。
/// 需要用户在系统设置里允许本应用「安装未知应用」；首次会跳转授权页。
///
/// 实现已整体搬到 Kotlin InstallBridge（Java 侧 try/catch，异常全文回传）：
/// Rust 逐条 JNI 调用的写法在异常时只剩 jni crate 的笼统一行字
/// （1.0.2–1.0.5 的 "Java exception was thrown ?" 即此因），无法定位
/// FileProvider 失败的真实原因。Rust 这里只做一次 loadClass + 无异常
/// 风险的静态调用，返回 String? = 失败原因（null = 成功）。
#[cfg(target_os = "android")]
#[tauri::command]
fn install_update(_app: tauri::AppHandle, path: String) -> Result<(), String> {
    use jni::objects::JObject;
    use android_bridge::{ACTIVITY, VM};

    let vm = VM.get().ok_or("native bridge 未初始化")?;
    let act_global = ACTIVITY.get().ok_or("activity 未初始化")?;
    let mut env = vm.attach_current_thread().map_err(|e| format!("attach jvm: {e}"))?;
    let ctx: &JObject = act_global.as_obj();

    // InstallBridge 类在 APK 的 PathClassLoader 里；attached 线程的 FindClass
    // 只走系统 classloader，应用类一律 ClassNotFoundException —— 必须经
    // activity.getClassLoader().loadClass（StatusBarThemeTask 同款模式）。
    let loader = env
        .call_method(ctx, "getClassLoader", "()Ljava/lang/ClassLoader;", &[])
        .map_err(|e| {
            let pending = jni_pending_exception(&mut env).unwrap_or_default();
            format!("getClassLoader: {e} {pending}")
        })?
        .l()
        .map_err(|e| e.to_string())?;
    let bridge_name = env
        .new_string("com/namelessric/dsh_mobile/InstallBridge")
        .map_err(|e| e.to_string())?;
    let bridge_cls = env
        .call_method(
            &loader,
            "loadClass",
            "(Ljava/lang/String;)Ljava/lang/Class;",
            &[(&bridge_name).into()],
        )
        .map_err(|e| {
            let pending = jni_pending_exception(&mut env).unwrap_or_default();
            format!("loadClass InstallBridge: {e} {pending}")
        })?
        .l()
        .map_err(|e| e.to_string())?;

    // String result = InstallBridge.installUpdate(activity, path)
    let path_j = env.new_string(&path).map_err(|e| e.to_string())?;
    let result = env
        .call_static_method(
            jni::objects::JClass::from(bridge_cls),
            "installUpdate",
            "(Landroid/app/Activity;Ljava/lang/String;)Ljava/lang/String;",
            &[ctx.into(), (&path_j).into()],
        )
        .map_err(|e| {
            let pending = jni_pending_exception(&mut env).unwrap_or_default();
            format!("InstallBridge.installUpdate 调用失败: {e} {pending}")
        })?;
    // 返回 null = 成功；非 null = Kotlin 侧整理好的失败原因全文。
    let result_obj = result.l().map_err(|e| e.to_string())?;
    if result_obj.is_null() {
        return Ok(());
    }
    let msg_j = jni::objects::JString::from(result_obj);
    let msg: String = env
        .get_string(&msg_j)
        .map_err(|e| e.to_string())?
        .into();
    Err(msg)
}

/// 取出上次进程留下的 Rust panic 记录（读后清空）。空串 = 没有记录。
/// 崩溃发生在 JS 起来之前时，这是唯一能带回现场的通路。
#[tauri::command]
fn read_crash_log(app: tauri::AppHandle) -> Result<String, String> {
    let dir = app
        .path()
        .app_log_dir()
        .or_else(|_| app.path().app_cache_dir())
        .map_err(|e| format!("crash log dir: {e}"))?;
    let mut out = String::new();
    let panic_path = dir.join("rust-panic.log");
    if let Ok(text) = std::fs::read_to_string(&panic_path) {
        out.push_str(&text);
        let _ = std::fs::remove_file(&panic_path);
    }
    // Kotlin 侧的生命周期记录（onCreate/onPause/onStop…）：定位"打开就切后台"
    // 到底发生在哪一步。读走即清，避免无限累积。
    // 通知链路诊断（NotifyBridge 写 cacheDir/dsh-notify.log）。
    let notify_path = app
        .path()
        .app_cache_dir()
        .map(|p| p.join("dsh-notify.log"))
        .unwrap_or_else(|_| dir.join("dsh-notify.log"));
    if let Ok(text) = std::fs::read_to_string(&notify_path) {
        if !out.is_empty() {
            out.push('\n');
        }
        out.push_str("-- notify --\n");
        out.push_str(&text);
        let _ = std::fs::remove_file(&notify_path);
    }
    // Kotlin 写的是 Context.cacheDir；必须直接用 app_cache_dir 定位，
    // 不能跟着上面 app_log_dir 的落点走。
    let life_path = app
        .path()
        .app_cache_dir()
        .map(|p| p.join("dsh-lifecycle.log"))
        .unwrap_or_else(|_| dir.join("dsh-lifecycle.log"));
    if let Ok(text) = std::fs::read_to_string(&life_path) {
        if !out.is_empty() {
            out.push('\n');
        }
        out.push_str("-- lifecycle --\n");
        out.push_str(&text);
        let _ = std::fs::remove_file(&life_path);
    }
    Ok(out)
}

/// 读系统剪贴板（图片或文本），返回 ClipboardBridge 组装的 JSON 字符串。
/// WebView 的 navigator.clipboard.read() 在本设备上被拒，原生读是唯一可靠路径。
#[cfg(target_os = "android")]
#[tauri::command]
fn read_clipboard() -> Result<String, String> {
    use jni::objects::{JObject, JString};
    use android_bridge::{ACTIVITY, VM};

    let vm = VM.get().ok_or("native bridge 未初始化")?;
    let act_global = ACTIVITY.get().ok_or("activity 未初始化")?;
    let mut env = vm.attach_current_thread().map_err(|e| format!("attach jvm: {e}"))?;
    let ctx: &JObject = act_global.as_obj();

    let loader = env
        .call_method(ctx, "getClassLoader", "()Ljava/lang/ClassLoader;", &[])
        .map_err(|e| format!("getClassLoader: {e}"))?
        .l()
        .map_err(|e| e.to_string())?;
    let name = env
        .new_string("com/namelessric/dsh_mobile/ClipboardBridge")
        .map_err(|e| e.to_string())?;
    let cls = env
        .call_method(
            &loader,
            "loadClass",
            "(Ljava/lang/String;)Ljava/lang/Class;",
            &[(&name).into()],
        )
        .map_err(|e| {
            let pending = jni_pending_exception(&mut env).unwrap_or_default();
            format!("loadClass ClipboardBridge: {e} {pending}")
        })?
        .l()
        .map_err(|e| e.to_string())?;
    let result = env
        .call_static_method(
            jni::objects::JClass::from(cls),
            "readClipboard",
            "(Landroid/app/Activity;)Ljava/lang/String;",
            &[ctx.into()],
        )
        .map_err(|e| {
            let pending = jni_pending_exception(&mut env).unwrap_or_default();
            format!("ClipboardBridge.readClipboard: {e} {pending}")
        })?;
    let obj = result.l().map_err(|e| e.to_string())?;
    if obj.is_null() {
        return Ok("{\"type\":\"empty\"}".into());
    }
    let s: String = env
        .get_string(&JString::from(obj))
        .map_err(|e| e.to_string())?
        .into();
    Ok(s)
}

/// 非 Android 平台的占位实现。
#[cfg(not(target_os = "android"))]
#[tauri::command]
fn read_clipboard() -> Result<String, String> {
    Err("read_clipboard 仅支持 Android".into())
}

/// 非 Android 平台的占位实现（桌面开发预览用不到）。
#[cfg(not(target_os = "android"))]
#[tauri::command]
fn install_update(_app: tauri::AppHandle, _path: String) -> Result<(), String> {
    Err("install_update 仅支持 Android".into())
}
