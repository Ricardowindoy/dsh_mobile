// dsh-mobile: a thin native shell around the dsh Web GUI. The bundled
// frontend collects the relay URL + access token, then navigates the
// WebView to the relay; everything else is plain web content.
//
// Config (relayUrl/accessToken) is persisted to a JSON file in the app
// config dir from the Rust side — WebView localStorage alone is not
// guaranteed to survive app restarts on Android.
//
// 应用内自动更新：download_update 把平台对应的安装包流式下载到应用缓存目录
// 并校验 SHA-256；Android 的 install_update 通过 JNI 构造 FileProvider
// content:// URI + ACTION_VIEW 拉起系统安装器（Android WebView/opener 插件
// 都无法直接装本地 APK，必须原生侧发 intent）；桌面平台直接打开下载好的
// 安装包（Windows setup.exe / macOS dmg / Linux deb）。

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
use std::time::{Duration, Instant};
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
        take_pending_session,
        desktop_cookie_sites,
        desktop_clear_site_cookies,
        desktop_wipe_web_data,
        update_background_start
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
        take_pending_session,
        android_cookie_sites,
        android_clear_site_cookies,
        android_wipe_web_data,
        update_background_start
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

/// 把远端/前端给的资产名收敛成一个安全的本地文件名：只取最后一段路径，
/// 只留 ASCII 字母数字与 . - _，拒绝空名和隐藏文件。正常资产名原样通过；
/// 异常输入回落 update.bin，绝不把 `../` 之类的路径带进缓存目录。
fn safe_update_filename(raw: Option<&str>) -> String {
    let name = raw
        .unwrap_or("")
        .rsplit(|c| c == '/' || c == '\\')
        .next()
        .unwrap_or("");
    let cleaned: String = name
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_'))
        .collect();
    if cleaned.is_empty() || cleaned.starts_with('.') {
        "update.bin".into()
    } else {
        cleaned
    }
}

/// 把更新包流式下载到应用缓存目录 updates/<资产名>，边下边算 SHA-256；
/// expected_sha256 非空时校验不匹配即报错并删除半成品。返回 {path,size}。
/// on_progress：必填的 tauri::ipc::Channel（tauri 2.12.5 没给 Option<Channel>
/// 的 CommandArg impl——Channel 只实现 Serialize），每个写入块都推
/// {received,total}；首尾事件保证「开始」与「100%」总会出现，不依赖分块边界。
///
/// filename：发布资产原名（JS 从三个源里真实携带）。桌面端靠扩展名选打开
/// 方式；relay 的下载 URL 末段是 download?platform=…，从 URL 推导不出文件名，
/// 所以前端必须传——URL 推导只是没传时的兜底（旧版前端）。
#[tauri::command]
async fn download_update(
    app: tauri::AppHandle,
    on_progress: tauri::ipc::Channel<DownloadProgress>,
    url: String,
    expected_sha256: Option<String>,
    filename: Option<String>,
) -> Result<Value, String> {
    let (final_path, size, digest) = download_to_cache(
        &app,
        &url,
        expected_sha256.as_deref(),
        filename.as_deref().filter(|s| !s.trim().is_empty()),
        |received, total| {
            // send 失败只意味着 WebView 已关闭/跳走：下载继续，不因进度上报失败而中断
            let _ = on_progress.send(DownloadProgress { received, total });
        },
    )
    .await?;
    Ok(json!({
        "path": final_path.to_string_lossy(),
        "size": size,
        "sha256": digest,
    }))
}

/// 共用的「流式下载到缓存目录 + 边下边算 SHA-256」实现。
/// download_update（用户点按钮的前台路径）与后台自动下载都走这里；
/// 差异只在进度回调（后台没有 WebView 在听，传空闭包）。
async fn download_to_cache(
    app: &tauri::AppHandle,
    url: &str,
    expected_sha256: Option<&str>,
    filename_hint: Option<&str>,
    mut on_chunk: impl FnMut(u64, Option<u64>),
) -> Result<(std::path::PathBuf, u64, String), String> {
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
        .get(url)
        .send()
        .await
        .map_err(|e| format!("连接更新服务器失败: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("更新服务器响应异常（HTTP {}）", resp.status()));
    }
    // Content-Length 有无决定前端进度是「定态百分比」还是「不定态」；
    // 有值也只作展示上界，真实进度以收到的字节为准（收到超量前端自行钳 100）。
    let total = resp.content_length().filter(|&t| t > 0);

    let dir = app
        .path()
        .app_cache_dir()
        .map_err(|e| format!("cache dir: {e}"))?
        .join("updates");
    tokio::fs::create_dir_all(&dir)
        .await
        .map_err(|e| format!("create update dir: {e}"))?;
    // 落盘名：资产原名优先（前台必传；后台从 BgUpdate.filename 传入），
    // 没有才从 URL 末段推导——relay 的 URL 末段是 download?platform=…，
    // 推导结果无扩展名，safe_update_filename 会把它收敛成 update.bin。
    let file_name = safe_update_filename(match filename_hint {
        Some(hint) => Some(hint),
        None => url.rsplit('/').next(),
    });
    let tmp_path = dir.join(format!("{file_name}.part"));
    let final_path = dir.join(&file_name);

    let mut hasher = sha2::Sha256::new();
    let mut file = tokio::fs::File::create(&tmp_path)
        .await
        .map_err(|e| format!("create temp file: {e}"))?;
    let mut size: u64 = 0;
    on_chunk(0, total);
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
        on_chunk(size, total);
    }
    file.flush().await.ok();
    drop(file);

    let digest: String = hasher.finalize().iter().map(|b| format!("{b:02x}")).collect();
    if let Some(expected) = expected_sha256.map(str::trim).filter(|s| !s.is_empty()) {
        if !digest.eq_ignore_ascii_case(expected) {
            let _ = tokio::fs::remove_file(&tmp_path).await;
            return Err("更新包完整性校验失败（SHA-256 不匹配），已放弃安装".into());
        }
    }
    // Windows 的 rename 不覆盖已存在目标：先删掉上一版同名安装包。
    let _ = tokio::fs::remove_file(&final_path).await;
    tokio::fs::rename(&tmp_path, &final_path)
        .await
        .map_err(|e| format!("finalize update file: {e}"))?;
    Ok((final_path, size, digest))
}

/// 更新检查结果在三个源之间共用的一小包元数据。
struct BgUpdate {
    version: String,
    url: String,
    sha256: String,
    filename: String,
}

/// 用用户偏好的源做一次更新检查（对应 JS 侧 checkUpdate 的三个源）。
/// 源不可用就报错返回——不做隐式回退（与 JS 侧同一约定：回退会让用户以为
/// 当前源能用）。
async fn bg_check_update(source: &str) -> Result<Option<BgUpdate>, String> {
    let version = app_version_string()?;
    let target = bg_platform_arch();
    match source {
        "gitee" => {
            let client = reqwest::Client::builder()
                .connect_timeout(Duration::from_secs(8))
                .timeout(Duration::from_secs(12))
                .build()
                .map_err(|e| format!("构建 HTTP 客户端失败: {e}"))?;
            let resp = client
                .get("https://gitee.com/api/v5/repos/namelessRic/dsh_mobile/releases?page=1&per_page=100")
                .header("Accept", "application/json")
                .send()
                .await
                .map_err(|e| format!("Gitee 不可达: {e}"))?;
            if !resp.status().is_success() {
                return Err(format!("Gitee HTTP {}", resp.status()));
            }
            let releases: Value = resp
                .json()
                .await
                .map_err(|e| format!("Gitee 响应解析失败: {e}"))?;
            let list = releases.as_array().cloned().unwrap_or_default();
            // Gitee releases 列表按创建时间升序返回，必须按版本挑最大
            // （JS 侧 pickLatestGiteeRelease 同一约定，取 arr[0] 会拿到最旧）。
            let mut best: Option<(&Value, String)> = None;
            for r in &list {
                let tag = bg_tag_of(r);
                let v = bg_normalize_version(&tag);
                if v.is_empty() {
                    continue;
                }
                if r.get("prerelease").and_then(Value::as_bool) == Some(true) {
                    continue;
                }
                if best.as_ref().map_or(true, |(_, bv)| bg_version_gt(&v, bv)) {
                    best = Some((r, v));
                }
            }
            let (release, ver) = match best {
                Some(x) => x,
                None => return Ok(None), // 该源暂无 release → 无更新
            };
            if bg_version_le(&ver, &version) {
                return Ok(None);
            }
            bg_pick_asset(release, &ver, "gitee", target).await
        }
        "relay" => {
            // relay 地址从持久化配置读：后台任务拿不到 WebView，config.json 是唯一入口。
            let relay = bg_relay_base(app_config_json().relay_url)?;
            let client = bg_relay_client(&relay)?;
            let url = format!(
                "{relay}/__dsh_tunnel__/update/check?platform={}&arch={}&appVersion={}",
                target.0,
                target.1,
                urlencode_version(&version),
            );
            let resp = client
                .get(&url)
                .send()
                .await
                .map_err(|e| format!("Relay 不可达: {e}"))?;
            if !resp.status().is_success() {
                return Err(format!("检查更新失败（HTTP {}）", resp.status()));
            }
            let body: Value = resp
                .json()
                .await
                .map_err(|e| format!("Relay 响应解析失败: {e}"))?;
            let code = body.get("code").and_then(Value::as_i64).unwrap_or(0);
            if code == 204 || code != 200 {
                return Ok(None);
            }
            let version_new = body
                .get("version")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string();
            let url = body.get("url").and_then(Value::as_str).unwrap_or("").to_string();
            if version_new.is_empty() || url.is_empty() {
                return Ok(None);
            }
            Ok(Some(BgUpdate {
                sha256: body.get("sha256").and_then(Value::as_str).unwrap_or("").to_string(),
                filename: body.get("filename").and_then(Value::as_str).unwrap_or("").to_string(),
                version: version_new,
                url,
            }))
        }
        // 缺省/未知源一律按 GitHub（与 JS 侧 normalizeSource 的回落一致）。
        _ => {
            let client = reqwest::Client::builder()
                .connect_timeout(Duration::from_secs(6))
                .timeout(Duration::from_secs(12))
                .user_agent("dsh-mobile-updater")
                .build()
                .map_err(|e| format!("构建 HTTP 客户端失败: {e}"))?;
            let resp = client
                .get("https://api.github.com/repos/Ricardowindoy/dsh_mobile/releases/latest")
                .header("Accept", "application/vnd.github+json")
                .send()
                .await
                .map_err(|e| format!("GitHub 不可达: {e}"))?;
            if !resp.status().is_success() {
                return Err(format!("GitHub HTTP {}", resp.status()));
            }
            let release: Value = resp
                .json()
                .await
                .map_err(|e| format!("GitHub 响应解析失败: {e}"))?;
            if release.get("draft").and_then(Value::as_bool) == Some(true)
                || release.get("prerelease").and_then(Value::as_bool) == Some(true)
            {
                return Ok(None);
            }
            let tag = bg_tag_of(&release);
            let ver = bg_normalize_version(&tag);
            if ver.is_empty() {
                return Err("GitHub release tag 异常".into());
            }
            if bg_version_le(&ver, &version) {
                return Ok(None);
            }
            bg_pick_asset(&release, &ver, "github", target).await
        }
    }
}

/// 从 GitHub/Gitee release JSON 里挑当前平台的安装包资产，映射成 BgUpdate。
async fn bg_pick_asset(
    release: &Value,
    version: &str,
    source: &str,
    target: (&str, &str),
) -> Result<Option<BgUpdate>, String> {
    let assets = release
        .get("assets")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    let (platform, arch) = target;
    // 与 JS 侧 PLATFORM_ASSET_RULES.android 同一优先级（后台自动更新只在
    // Android 上跑；桌面继续走前端 presentUpdate 流程）。
    let rules: [fn(&str) -> bool; 3] = [
        |n: &str| n.eq_ignore_ascii_case("app-arm64-release.apk"),
        |n: &str| {
            let lower = n.to_ascii_lowercase();
            lower.contains("arm64") && lower.ends_with(".apk")
        },
        |n: &str| n.to_ascii_lowercase().ends_with(".apk"),
    ];
    let mut asset: Option<&Value> = None;
    for rule in rules {
        if let Some(found) = assets.iter().find(|a| {
            a.get("name")
                .and_then(Value::as_str)
                .is_some_and(|n| rule(n))
        }) {
            asset = Some(found);
            break;
        }
    }
    let asset = match asset {
        Some(a) => a,
        None => return Err(format!("{source} release 没有 {platform}/{arch} 安装包")),
    };
    let name = asset.get("name").and_then(Value::as_str).unwrap_or("");
    let url = asset
        .get("browser_download_url")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();
    if url.is_empty() {
        return Err(format!("{source} release 资产缺少下载地址"));
    }
    // 校验文件只认 `<资产名>.sha256`：release 里同时有多平台的 .sha256，
    // 随便拿一个会让下载后的校验稳定失败（JS 侧同一约定）。
    let sha256 = bg_fetch_checksum_text(&url).await.unwrap_or_default();
    Ok(Some(BgUpdate {
        version: version.to_string(),
        url,
        sha256,
        filename: name.to_string(),
    }))
}

/// 下载 `<资产名>.sha256` 校验文件并取第一段 64 位 hex；拿不到就留空
/// （与 JS fillChecksum 同语义：仍可下载，只是没校验）。失败不报错。
async fn bg_fetch_checksum_text(asset_url: &str) -> Result<String, String> {
    let client = reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(6))
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| e.to_string())?;
    let resp = client
        .get(format!("{asset_url}.sha256"))
        .send()
        .await
        .map_err(|e| e.to_string())?;
    if !resp.status().is_success() {
        return Err(format!("checksum HTTP {}", resp.status()));
    }
    let text = resp.text().await.map_err(|e| e.to_string())?;
    for token in text.split_whitespace() {
        let t = token.trim_matches(|c: char| !c.is_ascii_hexdigit());
        if t.len() == 64 && t.chars().all(|c| c.is_ascii_hexdigit()) {
            return Ok(t.to_ascii_lowercase());
        }
    }
    Err("checksum 文件里没有 64 位 hex".into())
}

/// 后台任务里的版本比较：a > b。缺段按 0。
fn bg_version_gt(a: &str, b: &str) -> bool {
    bg_version_cmp(a, b) == std::cmp::Ordering::Greater
}

/// 后台任务里的版本比较：a <= b。
fn bg_version_le(a: &str, b: &str) -> bool {
    bg_version_cmp(a, b) != std::cmp::Ordering::Greater
}

fn bg_version_cmp(a: &str, b: &str) -> std::cmp::Ordering {
    let parse = |s: &str| -> Vec<u64> {
        bg_normalize_version(s)
            .split('.')
            .filter(|p| !p.is_empty())
            .map(|p| p.parse::<u64>().unwrap_or(0))
            .collect()
    };
    let (pa, pb) = (parse(a), parse(b));
    let n = pa.len().max(pb.len());
    for i in 0..n {
        let x = pa.get(i).copied().unwrap_or(0);
        let y = pb.get(i).copied().unwrap_or(0);
        if x != y {
            return x.cmp(&y);
        }
    }
    std::cmp::Ordering::Equal
}

/// 剥 v 前缀、留点分数字段（与 JS normalizeVersion 同规则，后台任务用）。
fn bg_normalize_version(raw: &str) -> String {
    let trimmed = raw.trim().trim_start_matches(['v', 'V']);
    let mut out = String::new();
    for c in trimmed.chars() {
        if c.is_ascii_digit() || c == '.' {
            out.push(c);
        } else {
            break;
        }
    }
    out.trim_matches('.').to_string()
}

fn bg_tag_of(release: &Value) -> String {
    release
        .get("tag_name")
        .or_else(|| release.get("name"))
        .and_then(Value::as_str)
        .unwrap_or("")
        .trim()
        .to_string()
}

/// 后台任务的目标平台：Android arm64（整条链路只在 Android 编译启用）。
fn bg_platform_arch() -> (&'static str, &'static str) {
    ("android", "arm64")
}

/// 配置文件的窄读面：后台任务只关心 relayUrl 与 updateSource。
struct BgConfig {
    relay_url: String,
    update_source: String,
}

/// 读 app_config_dir/config.json（save_config 的落点）。缺文件/解析失败返回
/// 空配置——后台更新是增益路径，配置读不到就不该把它变成错误弹给用户。
fn app_config_json() -> BgConfig {
    read_bg_config().unwrap_or(BgConfig {
        relay_url: String::new(),
        update_source: String::new(),
    })
}

#[cfg(target_os = "android")]
fn read_bg_config() -> Result<BgConfig, String> {
    use jni::objects::JObject;

    let vm = android_bridge::VM.get().ok_or("native bridge 未初始化")?;
    let act = android_bridge::ACTIVITY.get().ok_or("activity 未初始化")?;
    let mut env = vm
        .attach_current_thread()
        .map_err(|e| format!("attach jvm: {e}"))?;
    let activity: &JObject = act.as_obj();

    // Activity.getFilesDir() → File(dir, "config.json") 读原文。后台任务里
    // 拿不到前端通道，而 Android 上 app_config_dir 的真实落点就是
    // files/config.json（save_config 经 tauri::path() 写的那份）。
    let files_dir = env
        .call_method(activity, "getFilesDir", "()Ljava/io/File;", &[])
        .map_err(|e| format!("getFilesDir: {e}"))?
        .l()
        .map_err(|e| e.to_string())?;
    let name = env.new_string("config.json").map_err(|e| e.to_string())?;
    let fobj = env
        .new_object(
            "java/io/File",
            "(Ljava/io/File;Ljava/lang/String;)V",
            &[(&files_dir).into(), (&name).into()],
        )
        .map_err(|e| format!("new File: {e}"))?;
    let exists = env
        .call_method(&fobj, "exists", "()Z", &[])
        .map_err(|e| format!("exists: {e}"))?
        .z()
        .map_err(|e| e.to_string())?;
    if !exists {
        return Err("config.json 不存在".into());
    }
    let ret = env
        .call_method(&fobj, "getAbsolutePath", "()Ljava/lang/String;", &[])
        .map_err(|e| format!("getAbsolutePath: {e}"))?
        .l()
        .map_err(|e| e.to_string())?;
    let abs: String = env
        .get_string((&ret).into())
        .map_err(|e| e.to_string())?
        .to_string_lossy()
        .into_owned();
    let data = std::fs::read(&abs).map_err(|e| format!("read {abs}: {e}"))?;
    parse_bg_config(&data)
}

#[cfg(not(target_os = "android"))]
fn read_bg_config() -> Result<BgConfig, String> {
    Err("后台自动更新仅在 Android 上运行".into())
}

fn parse_bg_config(data: &[u8]) -> Result<BgConfig, String> {
    let v: Value = serde_json::from_slice(data).map_err(|e| format!("decode config: {e}"))?;
    Ok(BgConfig {
        relay_url: v.get("relayUrl").and_then(Value::as_str).unwrap_or("").to_string(),
        update_source: v.get("updateSource").and_then(Value::as_str).unwrap_or("").to_string(),
    })
}

fn bg_relay_base(relay_url: String) -> Result<String, String> {
    let base = relay_url.trim().trim_end_matches('/').to_string();
    if base.is_empty() {
        return Err("未配置 Relay 地址".into());
    }
    Ok(base)
}

/// relay 客户端：IP 入口显式注入内置私有 CA（reqwest/rustls 不读 Android 信任库）。
fn bg_relay_client(base: &str) -> Result<reqwest::Client, String> {
    let host = reqwest::Url::parse(base)
        .ok()
        .and_then(|u| u.host_str().map(str::to_string))
        .unwrap_or_default();
    let is_ip = host.parse::<std::net::IpAddr>().is_ok();
    let mut builder = reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(8))
        .timeout(Duration::from_secs(12));
    if is_ip {
        let root = reqwest::Certificate::from_pem(RELAY_CA_PEM)
            .map_err(|e| format!("内置 relay CA 解析失败: {e}"))?;
        builder = builder.add_root_certificate(root);
    }
    builder
        .build()
        .map_err(|e| format!("构建 HTTP 客户端失败: {e}"))
}

/// 版本串的 query 编码（appVersion 是纯版本号，只会出现点号）。
fn urlencode_version(v: &str) -> String {
    v.chars()
        .map(|c| match c {
            'A'..='Z' | 'a'..='z' | '0'..='9' | '-' | '_' | '.' | '~' => c.to_string(),
            _ => format!("%{:02X}", c as u32),
        })
        .collect()
}

/// 当前 App 版本：经 JNI 读 PackageInfo.versionName（后台任务拿不到 JS 侧
/// getVersion 通道）。读不到按空串处理——空串永远小于远端版本。
#[cfg(target_os = "android")]
fn app_version_string() -> Result<String, String> {
    let vm = android_bridge::VM.get().ok_or("native bridge 未初始化")?;
    let act = android_bridge::ACTIVITY.get().ok_or("activity 未初始化")?;
    let mut env = vm
        .attach_current_thread()
        .map_err(|e| format!("attach jvm: {e}"))?;
    let activity = act.as_obj();
    let pm = env
        .call_method(
            activity,
            "getPackageManager",
            "()Landroid/content/pm/PackageManager;",
            &[],
        )
        .map_err(|e| format!("getPackageManager: {e}"))?
        .l()
        .map_err(|e| e.to_string())?;
    let pkg = env
        .call_method(activity, "getPackageName", "()Ljava/lang/String;", &[])
        .map_err(|e| format!("getPackageName: {e}"))?
        .l()
        .map_err(|e| e.to_string())?;
    let info = env
        .call_method(
            &pm,
            "getPackageInfo",
            "(Ljava/lang/String;I)Landroid/content/pm/PackageInfo;",
            &[(&pkg).into(), jni::objects::JValue::Int(0)],
        )
        .map_err(|e| format!("getPackageInfo: {e}"))?
        .l()
        .map_err(|e| e.to_string())?;
    let vn = env
        .get_field(&info, "versionName", "Ljava/lang/String;")
        .map_err(|e| format!("versionName: {e}"))?
        .l()
        .map_err(|e| e.to_string())?;
    if vn.is_null() {
        return Ok(String::new());
    }
    let s: String = env
        .get_string((&vn).into())
        .map_err(|e| e.to_string())?
        .to_string_lossy()
        .into_owned();
    Ok(s.trim().to_string())
}

#[cfg(not(target_os = "android"))]
fn app_version_string() -> Result<String, String> {
    Err("后台自动更新仅在 Android 上运行".into())
}

/// 启动后台自动更新任务（fire-and-forget）。进程内幂等：已跑过就不再起。
/// 失败全部走 eprintln + Kotlin dsh-update.log，绝不打扰前台。
#[tauri::command]
fn update_background_start(app: tauri::AppHandle) -> Result<Value, String> {
    static STARTED: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);
    if STARTED.swap(true, std::sync::atomic::Ordering::SeqCst) {
        return Ok(json!({"started": false, "reason": "already running"}));
    }
    tauri::async_runtime::spawn(async move {
        if let Err(e) = update_background_run(&app).await {
            eprintln!("[dsh-update] background update failed: {e}");
        }
    });
    Ok(json!({"started": true}))
}

/// 同一版本 60s 内只弹一次窗（WebView 导航后 JS 不再介入，这里是唯一去重）。
static LAST_PROMPTED: std::sync::Mutex<Option<(String, Instant)>> = std::sync::Mutex::new(None);

/// 后台链路主体：检查(用户偏好源) → WiFi 门 → 下载 → 弹窗 → 安装。
/// 「跳过此版本」记在 WebView localStorage，后台任务读不到它——60s 去重是
/// 热启动兜底；冷启动再弹属可接受行为（用户可再点「稍后」）。
async fn update_background_run(app: &tauri::AppHandle) -> Result<(), String> {
    let cfg = app_config_json();
    let source = normalize_source_str(&cfg.update_source);

    let update = match bg_check_update(&source).await {
        Ok(Some(u)) => u,
        Ok(None) => return Ok(()), // 已是最新，静默结束
        Err(e) => return Err(e),
    };
    if let Ok(last) = LAST_PROMPTED.lock() {
        if let Some((ver, at)) = last.as_ref() {
            if ver == &update.version && at.elapsed() < Duration::from_secs(60) {
                return Ok(());
            }
        }
    }

    // WiFi 门：蜂窝/无网/判定失败都不自动下载（用户 2026-11-19 定）。
    if !bg_is_wifi().await? {
        eprintln!(
            "[dsh-update] not on wifi, skip background download v{}",
            update.version
        );
        return Ok(());
    }

    // 后台下载（进度无处可报也不需要报；SHA-256 与前台同一条路径）。
    let (path, size, _digest) = download_to_cache(
        app,
        &update.url,
        Some(update.sha256.as_str()),
        Some(update.filename.as_str()).filter(|s| !s.trim().is_empty()),
        |_, _| {},
    )
    .await?;
    eprintln!(
        "[dsh-update] downloaded v{} ({} bytes, {}) -> {}",
        update.version,
        size,
        update.filename,
        path.display()
    );

    // 弹窗询问（JNI → UpdateBridge.showPrompt，原生对话框）。
    if !bg_prompt_install(&update.version).await? {
        mark_prompted(&update.version);
        return Ok(());
    }
    mark_prompted(&update.version);
    // 立即安装：复用既有 install_update（Android 走 InstallBridge 的
    // FileProvider + PackageInstaller 双通道）。它是同步命令，直接调用。
    #[cfg(target_os = "android")]
    let install_result: Result<(), String> =
        install_update(app.clone(), path.to_string_lossy().into_owned());
    #[cfg(not(target_os = "android"))]
    let install_result: Result<(), String> = install_update(
        app.clone(),
        path.to_string_lossy().into_owned(),
    );
    install_result
}

fn mark_prompted(version: &str) {
    if let Ok(mut guard) = LAST_PROMPTED.lock() {
        *guard = Some((version.to_string(), Instant::now()));
    }
}

/// WiFi 判定：JNI → UpdateBridge.precheck（ConnectivityManager）。
/// false = 蜂窝/无网/判定失败，都不自动下载。
#[cfg(target_os = "android")]
async fn bg_is_wifi() -> Result<bool, String> {
    let raw = tokio::task::spawn_blocking(|| {
        call_update_bridge("precheck", "(Landroid/app/Activity;)Ljava/lang/String;", &[])
    })
    .await
    .map_err(|e| format!("spawn_blocking: {e}"))??;
    let v: Value = serde_json::from_str(&raw).map_err(|e| format!("precheck 响应解析失败: {e}"))?;
    Ok(v.get("wifi").and_then(Value::as_bool).unwrap_or(false))
}

#[cfg(not(target_os = "android"))]
async fn bg_is_wifi() -> Result<bool, String> {
    // 桌面无后台链路（前端 presentUpdate 流程保留），该函数不应被调用。
    Err("后台自动更新仅在 Android 上运行".into())
}

/// 弹窗询问：JNI → UpdateBridge.showPrompt。true = 用户点了「立即安装」。
/// Activity 由 call_update_bridge 自动打头（签名首参恒为 Activity）。
#[cfg(target_os = "android")]
async fn bg_prompt_install(version: &str) -> Result<bool, String> {
    let message = format!(
        "新版本 v{version} 已在后台下载完成（WiFi 网络，完整性校验通过）。是否立即安装？"
    );
    let raw = tokio::task::spawn_blocking(move || {
        call_update_bridge(
            "showPrompt",
            "(Landroid/app/Activity;Ljava/lang/String;)Ljava/lang/String;",
            &[&message],
        )
    })
    .await
    .map_err(|e| format!("spawn_blocking: {e}"))??;
    let v: Value =
        serde_json::from_str(&raw).map_err(|e| format!("showPrompt 响应解析失败: {e}"))?;
    Ok(v.get("install").and_then(Value::as_bool).unwrap_or(false))
}

#[cfg(not(target_os = "android"))]
async fn bg_prompt_install(_version: &str) -> Result<bool, String> {
    Err("后台自动更新仅在 Android 上运行".into())
}

/// UpdateBridge 的 JNI 调用路径：与 call_bridge 同一形状（取缓存类 → 现造
/// 字符串参数 → call_static_method → 读回 String），差异只有一点——每个
/// 调用的第一个实参恒为 Activity（precheck/showPrompt 的 Kotlin 签名都是
/// `fun x(activity: Activity, ...)`），这里自动拼在 str_args 前面。
/// 不做成第三个通用桥是因为两个调用面已完全覆盖，抽象面越小越好。
#[cfg(target_os = "android")]
fn call_update_bridge(method: &str, sig: &str, str_args: &[&str]) -> Result<String, String> {
    let vm = android_bridge::VM.get().ok_or("native bridge 未初始化")?;
    let act_global = android_bridge::ACTIVITY.get().ok_or("activity 未初始化")?;
    let mut env = vm.attach_current_thread().map_err(|e| format!("attach jvm: {e}"))?;
    let activity = act_global.as_obj();

    let outcome = (|| -> Result<String, String> {
        // attached 线程的 find_class 只看系统 classloader（1.0.24 真机教训），
        // 应用类必须走 nativeInitBridge 缓存的全局引用（同 COOKIE_CLASS）。
        let cls = bridge_class(&mut env, &android_bridge::UPDATE_CLASS, "UpdateBridge")?;
        // 字符串参数必须在本 env 里现造（JString 绑 env 的 local frame，
        // 跨 env 传是未定义行为）——与 call_bridge 同一约定。
        let mut jstr_locals: Vec<jni::objects::JString> = Vec::with_capacity(str_args.len());
        for a in str_args {
            let js = env.new_string(a).map_err(|e| format!("{method} 参数构造失败: {e}"))?;
            jstr_locals.push(js);
        }
        // JString 对 JObject 有 Deref，&JString 会自动强转成 &JObject（与
        // call_bridge 同一写法）；Activity 本身就是 &JObject。
        let mut jargs: Vec<jni::objects::JValue> = Vec::with_capacity(jstr_locals.len() + 1);
        jargs.push(jni::objects::JValue::Object(activity));
        for js in &jstr_locals {
            jargs.push(jni::objects::JValue::Object(js));
        }
        let ret = env
            .call_static_method(cls, method, sig, &jargs)
            .map_err(|e| format!("{method} 调用失败: {e}"))?;
        match ret {
            jni::objects::JValueOwned::Object(o) => {
                let s = env
                    .get_string((&o).into())
                    .map_err(|e| format!("{method} 返回值读取失败: {e}"))?;
                Ok(s.to_string_lossy().into_owned())
            }
            other => Err(format!("{method} 返回了非对象类型: {other:?}")),
        }
    })();

    match outcome {
        Ok(v) => Ok(v),
        Err(e) => {
            // jni crate 的 JavaException 不带类名/消息；这里补上并清掉挂起
            // 异常（不清会让 ART 回抛）——与 call_bridge 同一错误语义。
            let detail = jni_pending_exception(&mut env);
            let _ = env.exception_clear();
            Err(match detail {
                Some(d) => format!("UpdateBridge.{e}（Java: {d}）"),
                None => format!("UpdateBridge.{e}"),
            })
        }
    }
}

/// 下载源字符串的合法化：gitee/relay 之外一律 github（与 JS normalizeSource
/// 同一回落，后台任务用）。
fn normalize_source_str(raw: &str) -> String {
    match raw.trim().to_ascii_lowercase().as_str() {
        "gitee" => "gitee".into(),
        "relay" => "relay".into(),
        _ => "github".into(),
    }
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

// --- 登录 cookie 管理（桌面端） ---------------------------------------------
//
// 场景：GUI 的内嵌浏览器面板（iframe）里登录 JetHub 插件跳转的 Trae/WorkBuddy
// 等外部站点后，登录态落在本 App WebView 的 cookie store 里。用户切换账号时
// 需要按站点清掉对应 cookie —— 全清会把 DSH 自身的登录态一起杀掉，所以按站点。
//
// 平台差异（tauri 2.12 实读源码结论）：
//   · 桌面三平台 cookies()/cookies_for_url()/delete_cookie() 都可用，Cookie 带
//     domain()/path()（wry 各自的 cookie_from_* 转换都填了 domain）→ 这里直接
//     枚举全部 cookie、按 domain 分组、精确删除。
//   · Android：cookies() 恒返回空 Vec（wry android 未实现）→ 前端改走
//     android_cookie_* 命令（JNI → CookieManager），见 android_cookie_sites。

/// 归一化 cookie 域名用于分组展示与匹配：剥掉前导点，统一小写。
/// （`.example.com` 与 `example.com` 是同一个站点。）
/// 仅桌面路径使用（Android 走 JNI，域名归一在 Kotlin 侧）。
#[cfg(not(target_os = "android"))]
fn cookie_site(domain: &str) -> String {
    domain.trim_start_matches('.').to_lowercase()
}

#[cfg(not(target_os = "android"))]
#[tauri::command]
async fn desktop_cookie_sites(app: tauri::AppHandle) -> Result<Vec<Value>, String> {
    let window = app
        .get_webview_window("main")
        .ok_or("主窗口不存在")?;
    // cookies() 返回 store 里全部 cookie（含 HttpOnly）。Windows 上在同步
    // 命令里调用会死锁（wry 注释与 Webview2 issue 明说），必须 async 命令 +
    // spawn_blocking —— 这里已是 async 命令，但 wry 内部还要 pump 消息循环，
    // 再挪到独立线程最稳。
    tauri::async_runtime::spawn_blocking(move || {
        let cookies = window
            .cookies()
            .map_err(|e| format!("枚举 cookie 失败: {e}"))?;
        // 按 site 分组：site → {count, names}。name 列表给人看，同一站点
        // 多枚 cookie 折叠成一行。
        let mut sites: std::collections::BTreeMap<String, Vec<String>> =
            std::collections::BTreeMap::new();
        for c in &cookies {
            let domain = c.domain().unwrap_or("");
            if domain.is_empty() {
                continue;
            }
            sites
                .entry(cookie_site(domain))
                .or_default()
                .push(c.name().to_string());
        }
        Ok(sites
            .into_iter()
            .map(|(site, names)| {
                json!({ "site": site, "cookieCount": names.len(), "names": names })
            })
            .collect())
    })
    .await
    .map_err(|e| format!("枚举任务失败: {e}"))?
}

#[cfg(not(target_os = "android"))]
#[tauri::command]
async fn desktop_clear_site_cookies(
    app: tauri::AppHandle,
    site: String,
) -> Result<Value, String> {
    let target = cookie_site(&site);
    if target.is_empty() {
        return Err("站点名为空".into());
    }
    let window = app
        .get_webview_window("main")
        .ok_or("主窗口不存在")?;
    tauri::async_runtime::spawn_blocking(move || {
        let cookies = window
            .cookies()
            .map_err(|e| format!("枚举 cookie 失败: {e}"))?;
        // 精确匹配归一化域名：清 trae.net 不碰 my.trae.net 之外的子域策略
        // —— cookie domain 写的是什么就比什么（wry 返回的就是 store 里的
        // 原样 domain），不做后缀匹配，避免误删其它站点。
        let targets: Vec<_> = cookies
            .iter()
            .filter(|c| cookie_site(c.domain().unwrap_or("")) == target)
            .cloned()
            .collect();
        let targeted = targets.len();
        for c in &targets {
            // Tauri 的 WebviewWindow::delete_cookie 收的是**按值**的 Cookie
            // （wry 内部实现才是 &Cookie —— 照 wry 写会得到 E0308，CI 的
            // Windows/Linux/macOS/iOS 四个 job 会一起红）。
            let _ = window.delete_cookie(c.clone());
        }
        // 复核：删完再枚举一次，报「真正还剩几枚」而不是「我尝试了几次」。
        // Android 侧 1.0.26 就是拿调用次数当成功次数，日志写「cleared=121」
        // 而 cookie 一枚没少 —— 两个平台必须同一套诚实话术。
        let after = window
            .cookies()
            .map_err(|e| format!("复核枚举失败: {e}"))?;
        let remaining = after
            .iter()
            .filter(|c| cookie_site(c.domain().unwrap_or("")) == target)
            .count();
        let cleared = targeted.saturating_sub(remaining);
        Ok(json!({
            "attempts": targeted,
            "targeted": targeted,
            "cleared": cleared,
            "deleted": cleared,
            "remaining": remaining,
        }))
    })
    .await
    .map_err(|e| format!("清理任务失败: {e}"))?
}

/// 全量清理 WebView 浏览数据（桌面三平台）：cookie + localStorage + IndexedDB
/// + 缓存，一次清到底。用的是 wry 的 `clear_all_browsing_data`（各平台分别
/// 落到 `WebsiteDataManager.clear(ALL)` / `WKWebsiteDataStore.removeData` /
/// `ICoreWebView2Profile2.ClearBrowsingDataAll`）。
///
/// **配对信息不在其中**：relayUrl / deviceToken / deviceId 由 Rust 写在 app
/// 配置目录的 config.json 里，与 WebView 存储无关；清完之后前端用设备凭据换
/// 一次性 ticket 重新登录。
///
/// 复核：清完再枚举一次 cookie，如实报 `remaining`。桌面的 `cookies()` 是
/// 全量枚举（不像 Android 只能探清单），所以这里的数字是真实残留总量。
#[cfg(not(target_os = "android"))]
#[tauri::command]
async fn desktop_wipe_web_data(app: tauri::AppHandle) -> Result<Value, String> {
    let window = app
        .get_webview_window("main")
        .ok_or("主窗口不存在")?;
    tauri::async_runtime::spawn_blocking(move || {
        // Windows 上在同步上下文里碰 WebView2 会死锁（同 desktop_cookie_sites），
        // 所以整段都待在 spawn_blocking 里。
        window
            .clear_all_browsing_data()
            .map_err(|e| format!("清理浏览数据失败: {e}"))?;
        // 复核：清完再枚举，报真实残留而不是「我调用成功了」。
        let after = window
            .cookies()
            .map_err(|e| format!("复核枚举失败: {e}"))?;
        let mut hosts: Vec<String> = after
            .iter()
            .map(|c| cookie_site(c.domain().unwrap_or("")))
            .filter(|s| !s.is_empty())
            .collect();
        hosts.sort();
        hosts.dedup();
        Ok(json!({
            "ok": after.is_empty(),
            "remaining": after.len(),
            "remainingHosts": hosts,
        }))
    })
    .await
    .map_err(|e| format!("清理任务失败: {e}"))?
}

// --- Android：按站点清登录 cookie（JNI → CookieManager）---------------------
//
// 桌面用 webview 原生 cookie API（上面两个命令）；Android 的 wry 没实现
// cookies()（恒空 Vec），且系统 CookieManager 是唯一能读到 HttpOnly cookie
// 的入口 → 走 JNI 调 Kotlin CookieBridgeNative。
//
// JNI 包名转义：com.namelessric.dsh_mobile → com/namelessric/dsh_1mobile。

/// 取 CookieBridgeNative 的 Class 局部引用（缓存的是全局引用，每次要新 local）。
/// 走 nativeInitBridge 里 find_class 缓存的那份，**不再**用
/// `getClassLoader().loadClass()`：那条路要传点号二进制名，传 JNI 斜杠名会
/// ClassNotFoundException（1.0.24 真机事故），而且每次调用白做三次反射。
///
/// 两个桥共用这一条路径：传进来的是缓存槽（`OnceLock<GlobalRef>`）而不是
/// 类名，这样「哪个类」在编译期就定死了，也不会退化成按名字反射。
#[cfg(target_os = "android")]
fn bridge_class<'a>(
    env: &mut jni::JNIEnv<'a>,
    slot: &'static std::sync::OnceLock<jni::objects::GlobalRef>,
    label: &str,
) -> Result<jni::objects::JClass<'a>, String> {
    let global = slot
        .get()
        .ok_or_else(|| format!("{label} 类引用缺失（nativeInitBridge 未缓存？）"))?;
    let local = env
        .new_local_ref(global.as_obj())
        .map_err(|e| format!("{label} 类引用失效: {e}"))?;
    Ok(jni::objects::JClass::from(local))
}

#[cfg(target_os = "android")]
fn call_cookie_bridge(method: &str, sig: &str, str_args: &[&str]) -> Result<String, String> {
    call_bridge(
        "CookieBridgeNative",
        &android_bridge::COOKIE_CLASS,
        method,
        sig,
        str_args,
    )
}

#[cfg(target_os = "android")]
fn call_wipe_bridge(method: &str, sig: &str) -> Result<String, String> {
    call_bridge("WebDataWipeNative", &android_bridge::WIPE_CLASS, method, sig, &[])
}

/// 两个 JNI 桥的共同调用路径：取类 → 造字符串参数 → call_static_method →
/// 读回 String。差异只在「从哪个缓存槽取类引用」，槽由调用方传入。
#[cfg(target_os = "android")]
fn call_bridge<'a>(
    label: &str,
    slot: &'static std::sync::OnceLock<jni::objects::GlobalRef>,
    method: &str,
    sig: &str,
    str_args: &[&str],
) -> Result<String, String> {
    let vm = android_bridge::VM.get().ok_or("native bridge 未初始化")?;
    let mut env = vm.attach_current_thread().map_err(|e| format!("attach jvm: {e}"))?;

    let outcome = (|| -> Result<String, String> {
        let cls = bridge_class(&mut env, slot, label)?;
        // 字符串参数必须在本 env 里现造（JString 绑 env 的 local frame，
        // 跨 env 传是未定义行为），所以这一层统一收 &str 而不是 JValue。
        let jstrings = str_args
            .iter()
            .map(|a| env.new_string(a))
            .collect::<Result<Vec<_>, _>>()
            .map_err(|e| format!("{method} 参数构造失败: {e}"))?;
        let jargs: Vec<jni::objects::JValue> = jstrings
            .iter()
            // JString 对 JObject 有 Deref，&JString 会自动强转成 &JObject。
            .map(|s| jni::objects::JValue::Object(s))
            .collect();
        let ret = env
            .call_static_method(cls, method, sig, &jargs)
            .map_err(|e| format!("{method} 调用失败: {e}"))?;
        match ret {
            jni::objects::JValueOwned::Object(o) => {
                let s = env
                    .get_string((&o).into())
                    .map_err(|e| format!("{method} 返回值读取失败: {e}"))?;
                Ok(s.to_string_lossy().into_owned())
            }
            other => Err(format!("{method} 返回了非对象类型: {other:?}")),
        }
    })();

    match outcome {
        Ok(v) => Ok(v),
        Err(e) => {
            // jni crate 报 JavaException 时【不】带 Java 侧的类名与消息，只给
            // 一句 "Java exception was raised during method invocation" ——
            // 拿不到真因就没法定位（1.0.24 现场就是这样，日志里只有这一句）。
            // 这里补上类名+getMessage，并清掉挂起异常（不清会让 ART 回抛）。
            // label 也拼进去：现在有两个 JNI 桥，日志里必须能一眼看出是哪一个
            // 类挂的（同一个方法名 li 在别的类里可能有别的故障模式）。
            let detail = crate::jni_pending_exception(&mut env);
            let _ = env.exception_clear();
            Err(match detail {
                Some(d) => format!("{label}.{e}（Java: {d}）"),
                None => format!("{label}.{e}"),
            })
        }
    }
}

/// 列出 WebView cookie store 里有 cookie 的站点。
/// 返回 [{site, cookieCount, names[]}]。
#[cfg(target_os = "android")]
#[tauri::command]
fn android_cookie_sites() -> Result<Vec<Value>, String> {
    let raw = call_cookie_bridge("listSites", "()Ljava/lang/String;", &[])?;
    let parsed: Value = serde_json::from_str(&raw)
        .map_err(|e| format!("cookie 列表解析失败: {e}"))?;
    // Kotlin 侧不跨 JNI 抛异常，失败时回 {"error": "..."}。必须把它当错误往上
    // 报 —— 当成空数组的话，UI 会显示"没有保存登录 cookie 的站点"，那是在撒谎。
    if let Some(err) = parsed.get("error").and_then(|v| v.as_str()) {
        return Err(err.to_string());
    }
    parsed
        .as_array()
        .cloned()
        .ok_or_else(|| format!("cookie 列表格式异常: {raw}"))
}

/// 清一个站点（及其子域）的 cookie。cookie_names 空 = 该域全部。
/// 返回 {requested, cleared}。
#[cfg(target_os = "android")]
#[tauri::command]
fn android_clear_site_cookies(site: String, cookie_names: Vec<String>) -> Result<Value, String> {
    if site.trim().is_empty() {
        return Err("站点名为空".into());
    }
    let names_json = serde_json::to_string(&cookie_names)
        .map_err(|e| format!("序列化 cookie 名失败: {e}"))?;
    // 参数在 call_cookie_bridge 内部现造 —— 这里只给 &str，不碰 env。
    let raw = call_cookie_bridge(
        "clearSite",
        "(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;",
        &[&site, &names_json],
    )?;
    let parsed: Value = serde_json::from_str(&raw)
        .map_err(|e| format!("清理结果解析失败: {e}"))?;
    if let Some(err) = parsed.get("error").and_then(|v| v.as_str()) {
        return Err(err.to_string());
    }
    Ok(parsed)
}

/// 全量清理 WebView 站点数据（cookie / localStorage / IndexedDB / 表单与
/// HTTP 认证缓存 / 网页缓存），**配对信息不在其中**。
///
/// 与 [android_clear_site_cookies] 的分工是刻意的：那条按站点清（清不掉清单
/// 外的站点），这条一次清到底，代价是 dsh 自己的登录 cookie 也会没 —— 前端
/// 清完之后用设备凭据换新 ticket 重新登录，所以用户不用重新扫码配对。
///
/// 返回 Kotlin 的复核结果原文：`{ok, storageCleared, formCleared, remaining,
/// remainingHosts}`。`ok` 只在复查确实没有残留 cookie 时才为 true。
#[cfg(target_os = "android")]
#[tauri::command]
fn android_wipe_web_data() -> Result<Value, String> {
    let raw = call_wipe_bridge("wipeAll", "()Ljava/lang/String;")?;
    let parsed: Value = serde_json::from_str(&raw)
        .map_err(|e| format!("清理结果解析失败: {e}"))?;
    if let Some(err) = parsed.get("error").and_then(|v| v.as_str()) {
        return Err(err.to_string());
    }
    Ok(parsed)
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
    /// CookieBridgeNative 的 Class 全局引用。同 NOTIFY_CLASS：必须在
    /// nativeInitBridge（Java 主线程）里 find_class 缓存 —— attached 线程的
    /// find_class 只看系统 classloader，看不到应用类。
    /// ⚠️ 别改回「getClassLoader().loadClass(名字)」那套：`ClassLoader.loadClass`
    /// 要的是点号二进制名（com.a.B），传 JNI 斜杠名（com/a/B）会抛
    /// ClassNotFoundException —— 1.0.24 就是这么在真机上挂的。
    pub static COOKIE_CLASS: OnceLock<GlobalRef> = OnceLock::new();
    /// WebDataWipeNative（全量清理）的 Class 全局引用。规则与 COOKIE_CLASS 相同：
    /// 必须在 nativeInitBridge 里 find_class 缓存，attached 线程找不到应用类。
    pub static WIPE_CLASS: OnceLock<GlobalRef> = OnceLock::new();
    /// UpdateBridge（后台自动更新：WiFi 判定 + 完成弹窗）的 Class 全局引用。
    /// 规则与 COOKIE_CLASS 相同：nativeInitBridge 主线程 find_class 缓存。
    pub static UPDATE_CLASS: OnceLock<GlobalRef> = OnceLock::new();

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
        // 同上：find_class 在这里能用应用 classloader；缓存给 cookie 命令复用。
        if let Ok(cls) = env.find_class("com/namelessric/dsh_mobile/CookieBridgeNative") {
            if let Ok(g) = env.new_global_ref(&cls) {
                let _ = COOKIE_CLASS.set(g);
            }
        }
        // 全量清理（WebDataWipe）同理：它是另一个类，不能复用上面的引用。
        if let Ok(cls) = env.find_class("com/namelessric/dsh_mobile/WebDataWipeNative") {
            if let Ok(g) = env.new_global_ref(&cls) {
                let _ = WIPE_CLASS.set(g);
            }
        }
        // UpdateBridge（后台自动更新）同理：主线程 find_class 缓存。
        if let Ok(cls) = env.find_class("com/namelessric/dsh_mobile/UpdateBridge") {
            if let Ok(g) = env.new_global_ref(&cls) {
                let _ = UPDATE_CLASS.set(g);
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

/// 桌面平台：打开下载好的安装包（Windows 启动 setup.exe / macOS 挂载 dmg /
/// Linux 交给系统默认程序）。只允许打开缓存目录 updates/ 内的文件，避免
/// WebView 侧传入任意本地路径。Android 的 APK 安装走上面的 JNI 实现。
#[cfg(not(target_os = "android"))]
#[tauri::command]
fn install_update(app: tauri::AppHandle, path: String) -> Result<(), String> {
    let updates_dir = app
        .path()
        .app_cache_dir()
        .map_err(|e| format!("cache dir: {e}"))?
        .join("updates");
    let root = std::fs::canonicalize(&updates_dir)
        .map_err(|e| format!("更新缓存目录不可用：{e}"))?;
    let file = std::fs::canonicalize(&path)
        .map_err(|e| format!("安装包不存在或不可访问：{e}"))?;
    if !file.starts_with(&root) {
        return Err("安装包不在更新缓存目录，拒绝打开".into());
    }
    // 用原始绝对路径打开：Windows 的 canonicalize 会加 \\?\ 前缀，
    // ShellExecute 对扩展长度路径兼容性差。
    tauri_plugin_opener::open_path(&path, None::<&str>)
        .map_err(|e| format!("打开安装包失败：{e}"))
}
