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

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

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

/// 最后处理的 relay seq，跨 `notify_start` 调用保留（进程级）。
/// 没有它，每次 stop/start 都会以「无游标」重连，relay 会把最近 10 分钟的
/// 卡片再补投一遍（重复通知）。
static LAST_SEQ: AtomicU64 = AtomicU64::new(0);

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
    // 失败必须让前端知道：以前这里吞掉 JNI 异常仍返回 Ok，导致
    // 「notify subscription started」是假成功（R8 裁掉 start 后实测）。
    if let Err(e) = init_kotlin() {
        return Err(format!("通知渠道初始化失败: {e}"));
    }

    let handle = tauri::async_runtime::spawn(async move {
        let client = match crate::relay_client(&conn.relay_url) {
            Ok(c) => c,
            Err(e) => {
                eprintln!("[dsh-notify] client build failed: {e}");
                return;
            }
        };
        let base_url = format!(
            "{}/__dsh_tunnel__/notify/watch?deviceId={}",
            conn.relay_url,
            urlencode(&conn.device_id)
        );
        let mut backoff = Duration::from_secs(1);
        // 断点续传：记住最后收到的 seq，重连时带 ?after=N，relay 只补投之后
        // 的卡。没有它每次重连都会重放整个环形缓冲（用户实测：隔了两小时
        // 重连，一堆旧通知同时弹出）。0 = 首次连接，由 relay 按时间窗截断。
        let mut last_seq: u64 = LAST_SEQ.load(Ordering::Relaxed);
        while !stop.load(Ordering::SeqCst) {
            let url = if last_seq == 0 {
                base_url.clone()
            } else {
                format!("{base_url}&after={last_seq}")
            };
            match client
                .get(&url)
                .header("X-DSH-Relay-Token", &conn.device_token)
                .header("Accept", "text/event-stream")
                .send()
                .await
            {
                Ok(resp) if resp.status().is_success() => {
                    let connected_at = Instant::now();
                    let before = last_seq;
                    last_seq = last_seq.max(consume_sse(resp, &stop).await);
                    LAST_SEQ.store(last_seq, Ordering::Relaxed);
                    // 只有连接稳定存活过、或确实收到过新卡片，才把退避重置为 1s。
                    // 否则「连上就被踢」会变成 1s 一次的忙循环，把 relay 打爆。
                    if connected_at.elapsed() >= Duration::from_secs(5) || last_seq > before {
                        backoff = Duration::from_secs(1);
                    }
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
    if let Err(e) = kotlin_stop() {
        // 停订阅仍算成功（旧任务已 abort），但失败要留痕，别静默吞。
        eprintln!("[dsh-notify] kotlin_stop failed: {e}");
    }
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
async fn consume_sse(mut resp: reqwest::Response, stop: &Arc<AtomicBool>) -> u64 {
    let mut buf = String::new();
    let mut max_seq: u64 = 0;
    // 读空闲看门狗：服务端每 20s 一个 `: ping`，75s 收不到任何字节就判定
    // 这条连接已死（切后台/换网时 NAT 超时很常见，TCP 不会立刻报错，
    // `chunk()` 会永远挂着）。没有它 → 订阅静默失效，只有切回前台才恢复。
    const IDLE_LIMIT: Duration = Duration::from_secs(75);
    while !stop.load(Ordering::SeqCst) {
        let next = tokio::time::timeout(IDLE_LIMIT, resp.chunk()).await;
        let chunk_result = match next {
            Ok(r) => r,
            Err(_) => {
                eprintln!("[dsh-notify] watch idle {}s, reconnecting", IDLE_LIMIT.as_secs());
                return max_seq;
            }
        };
        match chunk_result {
            Ok(Some(chunk)) => {
                buf.push_str(&String::from_utf8_lossy(&chunk));
                while let Some(pos) = buf.find("\n\n") {
                    let frame = buf[..pos].to_string();
                    buf.drain(..pos + 2);
                    for line in frame.lines() {
                        if let Some(data) = line.strip_prefix("data: ") {
                            let parsed = serde_json::from_str::<Value>(data).ok();
                            if let Some(seq) = parsed.as_ref().and_then(|v| v.get("seq")).and_then(|x| x.as_u64()) {
                                max_seq = max_seq.max(seq);
                            }
                            if let Err(e) = push_to_kotlin(data) {
                                // 卡片送达 relay 不等于手机弹卡：JNI 失败必须可观测。
                                eprintln!("[dsh-notify] onCard failed: {e}");
                            } else if let Some(card_id) = parsed.as_ref().and_then(|v| v.get("id")).and_then(|x| x.as_str()) {
                                ack_card(card_id);
                            }
                        }
                    }
                }
                if buf.len() > 64 * 1024 {
                    buf.clear(); // 防御：畸形流不无限攒
                }
            }
            Ok(None) => return max_seq, // 服务端关闭 → 外层重连
            Err(_) => return max_seq,
        }
    }
    max_seq
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

/// JNI 桥调用结果：失败必须带原因，不能静默 `return`。
/// （2026-11-11 实测：release 包 R8 删掉 NotifyBridge.start 后，JNI 抛
/// NoSuchMethodError 被 exception_clear() 吞掉，notify_start 仍返回 Ok，
/// 排查时误判为「订阅成功」。判据必须落在可观测结果上。）
type JniResult<T> = Result<T, String>;

/// 取出并清掉 JNI 挂起异常；复用 lib.rs 的 helper 拿类名+message。
fn take_jni_exception(env: &mut jni::JNIEnv) -> String {
    let detail = crate::jni_pending_exception(env);
    // helper 已 clear；这里再兜一次，确保没有残留挂起异常让 ART 抛回 Java。
    let _ = env.exception_clear();
    detail.unwrap_or_else(|| "unknown JNI exception".into())
}

fn with_activity<F, T>(f: F) -> JniResult<T>
where
    F: FnOnce(&mut jni::JNIEnv, &jni::objects::JObject) -> JniResult<T>,
{
    let vm = crate::android_bridge::VM
        .get()
        .ok_or_else(|| "JavaVM 未初始化（nativeInitBridge 未调用？）".to_string())?;
    let activity = crate::android_bridge::ACTIVITY
        .get()
        .ok_or_else(|| "Activity 全局引用缺失".to_string())?;
    let mut env = vm
        .attach_current_thread()
        .map_err(|e| format!("attach JVM 失败: {e}"))?;
    let result = f(&mut env, activity.as_obj());
    // ⚠️ JNI 调用失败会在环境里留下 pending exception；不清掉的话，控制权
    // 回到 Java 时 ART 会把它抛出来，进程无征兆死亡（v1.0.9 实测：配对成功
    // 后 connect() 里第一次 notify_start → 进程 2 秒内被杀，用户看到"切后台"）。
    let _ = env.exception_clear();
    result
}

/// 取 NotifyBridge 类。attached 线程的 FindClass 只走系统 classloader，
/// 应用类必须用启动时缓存的全局引用（见 android_bridge::NOTIFY_CLASS）。
fn notify_bridge_class<'a>(
    env: &mut jni::JNIEnv<'a>,
) -> JniResult<jni::objects::JClass<'a>> {
    let global = crate::android_bridge::NOTIFY_CLASS
        .get()
        .ok_or_else(|| "NotifyBridge 类全局引用缺失（nativeInitBridge 未缓存？）".to_string())?;
    let local = env
        .new_local_ref(global.as_obj())
        .map_err(|e| format!("NotifyBridge 类引用失效: {e}"))?;
    Ok(jni::objects::JClass::from(local))
}

/// 调一个静态无返回值方法，并检查 JNI 是否真的成功了。
fn call_static_void(
    env: &mut jni::JNIEnv,
    cls: &jni::objects::JClass,
    name: &str,
    sig: &str,
    args: &[jni::objects::JValue],
) -> JniResult<()> {
    match env.call_static_method(cls, name, sig, args) {
        Ok(_) => {
            // call_static_method 返回 Ok 也可能带着挂起异常（Java 侧抛了，
            // jni crate 不自动转换成 Err）——必须显式 check。
            if env.exception_check().map_err(|e| e.to_string())? {
                Err(format!("{name} 抛异常: {}", take_jni_exception(env)))
            } else {
                Ok(())
            }
        }
        Err(e) => Err(format!(
            "{name} 调用失败: {e}; {}",
            take_jni_exception(env)
        )),
    }
}

/// 返回 Ok 仅当 Kotlin 侧渠道确实初始化成功。
fn init_kotlin() -> JniResult<()> {
    with_activity(|env, activity| {
        let cls = notify_bridge_class(env)?;
        call_static_void(
            env,
            &cls,
            "start",
            "(Landroid/content/Context;)V",
            &[jni::objects::JValue::Object(activity)],
        )
    })
}

fn kotlin_stop() -> JniResult<()> {
    with_activity(|env, _activity| {
        let cls = notify_bridge_class(env)?;
        call_static_void(env, &cls, "stop", "()V", &[])
    })
}

fn push_to_kotlin(json_data: &str) -> JniResult<()> {
    with_activity(|env, _activity| {
        let cls = notify_bridge_class(env)?;
        let s = env
            .new_string(json_data)
            .map_err(|e| format!("new_string 失败: {e}"))?;
        call_static_void(
            env,
            &cls,
            "onCard",
            "(Ljava/lang/String;)V",
            &[jni::objects::JValue::Object(&s)],
        )
    })
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

/// 卡片已渲染：向 relay 确认，之后不再补投。
fn ack_card(id: &str) {
    if id.is_empty() {
        return;
    }
    let Some(conn) = slot().conn.lock().unwrap().clone() else { return };
    let id = id.to_string();
    tauri::async_runtime::spawn(async move {
        let Ok(client) = crate::relay_client(&conn.relay_url) else { return };
        let resp = client
            .post(format!("{}/__dsh_tunnel__/notify/ack", conn.relay_url))
            .header("X-DSH-Relay-Token", &conn.device_token)
            .json(&json!({ "ids": [id] }))
            .send()
            .await;
        if let Ok(r) = resp {
            if !r.status().is_success() {
                eprintln!("[dsh-notify] ack HTTP {}", r.status());
            }
        }
    });
}

/// Kotlin AnswerReceiver 的结构化提问应答：answersJson 为
/// `[{"id":"scope","selected":["方案 A"]}]`。与审批应答走同一个 relay 端点，
/// 但载荷是 answers 数组而不是 decision —— relay 侧要求二者恰有其一。
#[no_mangle]
pub extern "system" fn Java_com_namelessric_dsh_1mobile_NotifyBridge_nativeAnswerJson(
    mut env: jni::JNIEnv,
    _class: jni::objects::JClass,
    id: jni::objects::JString,
    answers_json: jni::objects::JString,
) {
    let Ok(id) = env.get_string(&id).map(|s| s.to_string_lossy().into_owned()) else { return };
    let Ok(raw) = env.get_string(&answers_json).map(|s| s.to_string_lossy().into_owned()) else { return };
    // Validate before sending: the relay also validates, but a malformed local
    // payload would otherwise be rejected silently at the far end.
    let Ok(answers) = serde_json::from_str::<Value>(&raw) else {
        eprintln!("[dsh-notify] question answer is not JSON");
        return;
    };
    if !answers.is_array() {
        eprintln!("[dsh-notify] question answer must be an array");
        return;
    }
    let Some(conn) = slot().conn.lock().unwrap().clone() else { return };
    tauri::async_runtime::spawn(async move {
        let Ok(client) = crate::relay_client(&conn.relay_url) else { return };
        let resp = client
            .post(format!("{}/__dsh_tunnel__/notify/answer", conn.relay_url))
            .header("X-DSH-Relay-Token", &conn.device_token)
            .json(&json!({"id": id, "answers": answers}))
            .send()
            .await;
        match resp {
            Ok(r) => eprintln!("[dsh-notify] question answer HTTP {}", r.status()),
            Err(e) => eprintln!("[dsh-notify] question answer failed: {e}"),
        }
    });
}