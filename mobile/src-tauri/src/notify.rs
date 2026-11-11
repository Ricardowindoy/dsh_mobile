// 审批/提问推送：Rust 侧维持 relay SSE 订阅（WP4c）。
//
// 为什么在 Rust 而不是 JS：WebView 进 GUI 后是 relay 域的页面，我们的 JS 不
// 在那里跑；而且 WebView 切后台随时可能被冻结。Rust tokio 任务跟着 App 进程
// 活，SSE 断了自动重连（指数退避封顶 30s），每条消息经 JNI 推给 Kotlin
// NotifyBridge 弹原生通知。进程被杀则订阅消失 —— v1 无常驻服务（已知限制）。
//
// 凭据：设备 token 只进 header；SSE URL 只带 deviceId（路由键，非凭据）。
// 应答：Kotlin action 按钮 → nativeAnswer → 这里用同一设备 token POST。

#![cfg(target_os = "android")]

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use serde_json::{json, Value};
use tauri::async_runtime::JoinHandle;

/// 不可变连接事实：一次 start 建一份，任务与应答共用（Arc 克隆）。
struct NotifyConn {
    relay_url: String,
    device_id: String,
    device_token: String,
}

struct NotifySlot {
    conn: Mutex<Option<Arc<NotifyConn>>>,
    stop: Mutex<Arc<AtomicBool>>,
    task: Mutex<Option<JoinHandle<()>>>,
}

static SLOT: OnceLock<NotifySlot> = OnceLock::new();

fn slot() -> &'static NotifySlot {
    SLOT.get_or_init(|| NotifySlot {
        conn: Mutex::new(None),
        stop: Mutex::new(Arc::new(AtomicBool::new(true))),
        task: Mutex::new(None),
    })
}

/// 启动订阅（幂等：重复调用先停旧任务）。前端在进 GUI 前调用。
#[tauri::command]
pub async fn notify_start(
    relay_url: String,
    device_id: String,
    device_token: String,
) -> Result<Value, String> {
    if device_token.is_empty() || device_id.is_empty() {
        return Err("缺少设备凭据".into());
    }
    notify_stop_inner();
    let conn = Arc::new(NotifyConn {
        relay_url: relay_url.trim_end_matches('/').to_string(),
        device_id,
        device_token,
    });
    // stop 标志每代一份：旧任务持旧 Arc，新任务持新 Arc —— stop_inner 只翻
    // 当前代，不会出现「新任务刚起就被旧 stop 标志杀死」的竞态。
    let stop = Arc::new(AtomicBool::new(false));
    {
        let s = slot();
        *s.conn.lock().unwrap() = Some(conn.clone());
        *s.stop.lock().unwrap() = stop.clone();
    }

    // Kotlin 侧初始化通知渠道（经既有 JNI 桥拿 Activity）。
    init_kotlin();

    let handle = tauri::async_runtime::spawn(async move {
        let client = match crate::relay_client(&conn.relay_url) {
            Ok(c) => c,
            Err(e) => {
                eprintln!("[dsh-notify] client build failed: {e}");
                return;
            }
        };
        let url = format!(
            "{}/__dsh_tunnel__/notify/watch?deviceId={}",
            conn.relay_url,
            urlencode(&conn.device_id)
        );
        let mut backoff = Duration::from_secs(1);
        while !stop.load(Ordering::SeqCst) {
            match client
                .get(&url)
                .header("X-DSH-Relay-Token", &conn.device_token)
                .header("Accept", "text/event-stream")
                .send()
                .await
            {
                Ok(resp) if resp.status().is_success() => {
                    backoff = Duration::from_secs(1);
                    consume_sse(resp, &stop).await;
                }
                Ok(resp) => {
                    eprintln!("[dsh-notify] watch HTTP {}", resp.status());
                    if resp.status() == reqwest::StatusCode::UNAUTHORIZED {
                        return; // 凭据被撤销：不再重连
                    }
                }
                Err(e) => eprintln!("[dsh-notify] watch error: {e}"),
            }
            if stop.load(Ordering::SeqCst) {
                return;
            }
            tokio::time::sleep(backoff).await;
            backoff = (backoff * 2).min(Duration::from_secs(30));
        }
    });
    *slot().task.lock().unwrap() = Some(handle);
    Ok(json!({"started": true}))
}

/// 停止订阅并撤下卡片。
#[tauri::command]
pub async fn notify_stop() -> Result<Value, String> {
    notify_stop_inner();
    kotlin_stop();
    Ok(json!({"stopped": true}))
}

fn notify_stop_inner() {
    let s = slot();
    s.stop.lock().unwrap().store(true, Ordering::SeqCst);
    if let Some(task) = s.task.lock().unwrap().take() {
        task.abort();
    }
}

/// SSE 帧解析：`data: {json}\n\n`；`: ping` 注释行忽略。
async fn consume_sse(mut resp: reqwest::Response, stop: &Arc<AtomicBool>) {
    let mut buf = String::new();
    while !stop.load(Ordering::SeqCst) {
        match resp.chunk().await {
            Ok(Some(chunk)) => {
                buf.push_str(&String::from_utf8_lossy(&chunk));
                while let Some(pos) = buf.find("\n\n") {
                    let frame = buf[..pos].to_string();
                    buf.drain(..pos + 2);
                    for line in frame.lines() {
                        if let Some(data) = line.strip_prefix("data: ") {
                            push_to_kotlin(data);
                        }
                    }
                }
                if buf.len() > 64 * 1024 {
                    buf.clear(); // 防御：畸形流不无限攒
                }
            }
            Ok(None) => return, // 服务端关闭 → 外层重连
            Err(_) => return,
        }
    }
}

fn urlencode(s: &str) -> String {
    s.bytes()
        .map(|b| match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                (b as char).to_string()
            }
            _ => format!("%{b:02X}"),
        })
        .collect()
}

// ── JNI 桥 ────────────────────────────────────────────────────────────

fn with_activity<F: FnOnce(&mut jni::JNIEnv, &jni::objects::JObject)>(f: F) {
    let Some(vm) = crate::android_bridge::VM.get() else { return };
    let Some(activity) = crate::android_bridge::ACTIVITY.get() else { return };
    let Ok(mut env) = vm.attach_current_thread() else { return };
    f(&mut env, activity.as_obj());
    // ⚠️ JNI 调用失败会在环境里留下 pending exception；不清掉的话，控制权
    // 回到 Java 时 ART 会把它抛出来，进程无征兆死亡（v1.0.9 实测：配对成功
    // 后 connect() 里第一次 notify_start → 进程 2 秒内被杀，用户看到"切后台"）。
    let _ = env.exception_clear();
}

/// 取 NotifyBridge 类。attached 线程的 FindClass 只走系统 classloader，
/// 应用类必须用启动时缓存的全局引用（见 android_bridge::NOTIFY_CLASS）。
fn notify_bridge_class<'a>(env: &mut jni::JNIEnv<'a>) -> Option<jni::objects::JClass<'a>> {
    let global = crate::android_bridge::NOTIFY_CLASS.get()?;
    let local = env.new_local_ref(global.as_obj()).ok()?;
    Some(jni::objects::JClass::from(local))
}

fn init_kotlin() {
    with_activity(|env, activity| {
        let Some(cls) = notify_bridge_class(env) else { return };
        let _ = env.call_static_method(
            cls,
            "start",
            "(Landroid/content/Context;)V",
            &[jni::objects::JValue::Object(activity)],
        );
    });
}

fn kotlin_stop() {
    with_activity(|env, _activity| {
        let Some(cls) = notify_bridge_class(env) else { return };
        let _ = env.call_static_method(cls, "stop", "()V", &[]);
    });
}

fn push_to_kotlin(json_data: &str) {
    with_activity(|env, _activity| {
        let Some(cls) = notify_bridge_class(env) else { return };
        let Ok(s) = env.new_string(json_data) else { return };
        let _ = env.call_static_method(
            cls,
            "onCard",
            "(Ljava/lang/String;)V",
            &[jni::objects::JValue::Object(&s)],
        );
    });
}

/// Kotlin AnswerReceiver 的 native 实现：设备 token POST 应答。
/// 包名段 com/namelessric/dsh_1mobile（下划线转义）。
#[no_mangle]
pub extern "system" fn Java_com_namelessric_dsh_1mobile_NotifyBridge_nativeAnswer(
    mut env: jni::JNIEnv,
    _class: jni::objects::JClass,
    id: jni::objects::JString,
    decision: jni::objects::JString,
) {
    let Ok(id) = env.get_string(&id).map(|s| s.to_string_lossy().into_owned()) else { return };
    let Ok(decision) = env.get_string(&decision).map(|s| s.to_string_lossy().into_owned()) else { return };
    if decision != "allowed-once" && decision != "rejected" {
        return;
    }
    let Some(conn) = slot().conn.lock().unwrap().clone() else { return };
    tauri::async_runtime::spawn(async move {
        let Ok(client) = crate::relay_client(&conn.relay_url) else { return };
        let resp = client
            .post(format!("{}/__dsh_tunnel__/notify/answer", conn.relay_url))
            .header("X-DSH-Relay-Token", &conn.device_token)
            .json(&json!({"id": id, "decision": decision}))
            .send()
            .await;
        match resp {
            Ok(r) => eprintln!("[dsh-notify] answer HTTP {}", r.status()),
            Err(e) => eprintln!("[dsh-notify] answer failed: {e}"),
        }
    });
}