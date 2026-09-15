//! WorkBuddy 国内版/国际版活动中心自动化引擎。
//!
//! 移植自 `F:\R\workbuddy2api-intl`（wb_fingerprint.py / wb_tasks.py）与
//! `F:\R\workbuddy2api` 的活动协议：
//!
//! * **稳定设备指纹**：以账号 uid + 固定业务盐值做单向哈希派生 machineId /
//!   sessionId / requestId。同一账号永远来自同一台「虚拟物理设备」，多账号之间
//!   天然隔离 —— 避免随机机器码漂移触发风控，也避免跨账号关联。
//! * **成长任务/积分任务**：查询 → 批量接取 → 构造规范行为事件上报点亮 → 领奖入账。
//! * **猫猫旅行**：查状态 → idle 派出 / arrived 领奖。
//!
//! 所有出站请求强制 >= 1s 间隔（防风控），并复用稳定指纹。

use std::time::Duration;

use chrono::Timelike;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::modules::{logger, workbuddy_account};
use crate::models::workbuddy::WorkbuddyAccount;

// ── 域名与 UA ─────────────────────────────────────────────────
pub const CN_CHAT_BASE: &str = "https://copilot.tencent.com";
pub const CN_BILL_BASE: &str = "https://www.codebuddy.cn";
pub const CN_WEB_BASE: &str = "https://www.workbuddy.cn";
pub const GLOBAL_CHAT_BASE: &str = "https://www.workbuddy.ai";
pub const GLOBAL_BILL_BASE: &str = "https://www.workbuddy.ai";
const DESKTOP_UA: &str = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1";
const WEB_UA: &str = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36";
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
/// 相邻出站请求的最小间隔，防风控。
pub const MIN_REQUEST_GAP: Duration = Duration::from_millis(1000);

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum WorkbuddyRealm {
    Cn,
    Global,
}

impl WorkbuddyRealm {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Cn => "cn",
            Self::Global => "global",
        }
    }

    pub fn chat_base(self) -> &'static str {
        match self {
            Self::Cn => CN_CHAT_BASE,
            Self::Global => GLOBAL_CHAT_BASE,
        }
    }

    pub fn bill_base(self) -> &'static str {
        match self {
            Self::Cn => CN_BILL_BASE,
            Self::Global => GLOBAL_BILL_BASE,
        }
    }

    pub fn origin(self) -> &'static str {
        match self {
            Self::Cn => CN_BILL_BASE,
            Self::Global => GLOBAL_BILL_BASE,
        }
    }

    pub fn accept_language(self) -> &'static str {
        match self {
            Self::Cn => "zh-CN",
            Self::Global => "en-US",
        }
    }
}

/// 按账号 domain 判定 realm：`workbuddy.cn` / `codebuddy.cn` 系为国内版。
pub fn resolve_realm(domain: Option<&str>) -> WorkbuddyRealm {
    let value = domain.unwrap_or_default().trim().to_ascii_lowercase();
    if value.is_empty() || value.contains("codebuddy.cn") || value.contains("workbuddy.cn") {
        return WorkbuddyRealm::Cn;
    }
    if value.contains("workbuddy.ai") || value.contains("codebuddy.ai") {
        return WorkbuddyRealm::Global;
    }
    WorkbuddyRealm::Cn
}

// ── 稳定设备指纹 (derive_id) ───────────────────────────────────
/// 由 `uid + salt` 稳定派生十六进制标识（MD5 hexdigest，32 字符）。
///
/// 与 Python `hashlib.md5(...).hexdigest()[:36]` 对齐：hexdigest 仅 32 位，
/// `[:36]` 实际等价于取全串。幂等：同一账号每次调用结果相同。
pub fn derive_id(uid: &str, salt: &str) -> String {
    let identity = if uid.trim().is_empty() { "anonymous" } else { uid };
    let seed = format!("{}:{}", salt, identity);
    format!("{:x}", md5::compute(seed.as_bytes()))
}

/// 带稳定前缀 + 微秒时间戳的 X-Request-ID（前缀稳定 = 同一设备，后缀唯一 = 不重放）。
pub fn generate_request_id(uid: &str) -> String {
    let suffix = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|value| value.subsec_micros())
        .unwrap_or(0)
        % 1_000_000;
    format!("{}-{:06}", derive_id(uid, "req"), suffix)
}

pub fn machine_id(uid: &str) -> String {
    derive_id(uid, "machine")
}

pub fn session_id(uid: &str) -> String {
    derive_id(uid, "session")
}

/// 事件上报所需的标准桌面端指纹（字段对齐官方客户端）。
pub fn desktop_fingerprint(uid: &str, nickname: &str) -> Value {
    let now = chrono::Utc::now().timestamp_millis();
    json!({
        "timezone": "Asia/Shanghai",
        "reportDelay": 2000,
        "userId": uid,
        "username": nickname,
        "userNickname": nickname,
        "product": "SaaS",
        "releaseDate": 1789036585355i64,
        "commit": "5f9692923c93033111c51ad7b003eb80204a9b75",
        "ideName": "WorkBuddy",
        "ideType": "WorkBuddy",
        "ideVersion": "5.5.6",
        "machineId": machine_id(uid),
        "sessionId": session_id(uid),
        "extName": "workbuddy-desktop",
        "extVersion": "5.5.6",
        "os": "win32",
        "arch": "x64",
        "osVersion": "10.0.26220",
        "cpuCores": 20,
        "memorySize": 24,
        "timestamp": now,
        "presentAt": now,
    })
}

// ── 出站请求头（双域适配） ─────────────────────────────────────
fn outbound_headers(account: &WorkbuddyAccount, realm: WorkbuddyRealm) -> Vec<(&'static str, String)> {
    let origin = realm.origin();
    let uid = account.uid.clone().unwrap_or_default();
    let mut headers = vec![
        ("Content-Type", "application/json".to_string()),
        ("Accept", "application/json".to_string()),
        ("Accept-Language", realm.accept_language().to_string()),
        ("X-Requested-With", "XMLHttpRequest".to_string()),
        ("X-CodeBuddy-Request", "1".to_string()),
        ("User-Agent", DESKTOP_UA.to_string()),
        ("Origin", origin.to_string()),
        ("Referer", format!("{}/", origin)),
        ("X-Agent-Purpose", "conversation".to_string()),
        ("X-IDE-Name", "WorkBuddy".to_string()),
        ("X-IDE-Type", "WorkBuddy".to_string()),
        ("X-IDE-Version", "5.5.6".to_string()),
        ("X-Product", "WorkBuddy".to_string()),
        ("Authorization", format!("Bearer {}", account.access_token)),
    ];
    if !uid.trim().is_empty() {
        headers.push(("X-User-Id", uid.clone()));
        // 稳定设备指纹头：同一账号恒定，多账号隔离。
        headers.push(("X-Machine-Id", machine_id(&uid)));
        headers.push(("X-Session-Id", session_id(&uid)));
        headers.push(("X-Request-Id", generate_request_id(&uid)));
    }
    if let Some(enterprise_id) = account.enterprise_id.as_deref().filter(|v| !v.trim().is_empty()) {
        headers.push(("X-Enterprise-Id", enterprise_id.to_string()));
    }
    if let Some(domain) = account.domain.as_deref().filter(|v| !v.trim().is_empty()) {
        headers.push(("X-Domain", domain.to_string()));
    }
    headers
}

/// 网页域领奖用的头（desktop 领奖 400 时降级）。
fn web_claim_headers(account: &WorkbuddyAccount) -> Vec<(&'static str, String)> {
    let uid = account.uid.clone().unwrap_or_default();
    let mut headers = vec![
        ("Authorization", format!("Bearer {}", account.access_token)),
        ("Accept", "application/json, text/plain, */*".to_string()),
        ("Content-Type", "application/json".to_string()),
        ("Origin", CN_WEB_BASE.to_string()),
        (
            "Referer",
            format!("{}/profile/growth-center", CN_WEB_BASE),
        ),
        ("x-client-platform", "web".to_string()),
        ("User-Agent", WEB_UA.to_string()),
        ("X-Domain", CN_WEB_BASE.to_string()),
    ];
    if !uid.trim().is_empty() {
        headers.push(("X-User-Id", uid));
    }
    headers
}

// ── 模型 ──────────────────────────────────────────────────────
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GrowthTask {
    pub task_code: String,
    pub name: String,
    pub description: String,
    /// not_accepted / accepted / completed / claimed
    pub status: String,
    pub current: i64,
    pub target: i64,
    pub reward_credit: i64,
    pub reward_energy: i64,
    /// 无法伪造（真实捐赠等），自动化跳过。
    pub unforgeable: bool,
    pub reason: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ActivityRunLog {
    pub ok: bool,
    pub account_id: String,
    pub label: String,
    pub earned_credit: i64,
    pub logs: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TravelStatus {
    pub state: String,
    pub daily_limit_reached: bool,
    pub raw: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ActivityOverview {
    pub ok: bool,
    pub realm: WorkbuddyRealm,
    pub energy: i64,
    pub streak_days: i64,
    pub travel: TravelStatus,
    pub tasks: Vec<GrowthTask>,
    pub message: String,
}

// ── 任务规格 ──────────────────────────────────────────────────
struct TaskSpec {
    code: &'static str,
    kind: &'static str,
    target: i64,
    reward: i64,
    name: &'static str,
    unforgeable: bool,
    reason: &'static str,
}

macro_rules! spec {
    ($code:expr, $kind:expr, $target:expr, $reward:expr, $name:expr) => {
        TaskSpec {
            code: $code,
            kind: $kind,
            target: $target,
            reward: $reward,
            name: $name,
            unforgeable: false,
            reason: "",
        }
    };
}

const TASK_SPECS: &[TaskSpec] = &[
    spec!("create_canvas", "canvas", 1, 300, "创建设计任务"),
    spec!("template_5", "template", 5, 200, "模板创建任务"),
    spec!("expert_5", "expert", 5, 200, "使用专家助手"),
    spec!("Expert_team_use_3", "team", 3, 150, "使用专家团队"),
    spec!("skill_1", "skill", 1, 100, "体验技能"),
    spec!("automation_1", "automation", 1, 100, "创建自动化任务"),
    spec!("playbook_prompt", "playbook", 1, 100, "灵感案例使用"),
    spec!("Expert_lighthouse", "lighthouse", 1, 100, "轻量云专家使用"),
    spec!("Buddy_App", "buddy5", 1, 100, "进入 Buddy 应用"),
    spec!("Buddy_App_QQ", "buddy5", 1, 100, "企鹅教师助手"),
    spec!("Hp_Appearance", "skin", 1, 100, "应用主题外观"),
    spec!("chat_5", "chat", 5, 100, "发起 5 次对话"),
    spec!("Model_chat_GLM5.2", "glmchat", 1, 100, "体验 GLM-5.2"),
    spec!("black_cat", "cat", 3, 100, "夜猫子任务 (23:00-08:00)"),
    spec!("RichMeow_Chat", "richmeow", 1, 100, "桌面对话事件链"),
    spec!("Library_read", "library", 1, 100, "浏览资料库"),
    spec!("first_buddy", "buddy_first", 1, 0, "领养首只猫猫"),
    TaskSpec {
        code: "Expert_Philanthropy",
        kind: "",
        target: 1,
        reward: 0,
        name: "公益爱心捐赠",
        unforgeable: true,
        reason: "真实捐款动作",
    },
];

fn spec_for(code: &str) -> Option<&'static TaskSpec> {
    TASK_SPECS.iter().find(|spec| spec.code == code)
}

fn kind_for(code: &str) -> Option<&'static str> {
    spec_for(code).map(|spec| spec.kind)
}

// ── 事件构造 ──────────────────────────────────────────────────
/// 构造指定类型的「真实规范行为事件」。字段对齐官方客户端上报口径。
pub fn build_event(uid: &str, kind: &str, idx: usize) -> Value {
    let now = chrono::Utc::now().timestamp_millis();
    let cid = format!("wb-task-{}-{}", now, idx);
    let rid = format!("{}-req", cid);
    match kind {
        "canvas" => json!({
            "eventCode": "wbx_design_canvas_task_create", "timestamp": now, "reportDelay": 0,
            "conversationId": cid, "requestId": rid, "source": "summon_keyword",
            "isCustomModel": false, "name": "", "inputLength": 12,
            "id": format!("wbx-canvas-{}", now), "cost": 0, "isSuccessful": true, "userId": uid
        }),
        "template" => json!({
            "eventCode": "agent_task_created_with_template", "timestamp": now, "reportDelay": 0,
            "isCustomModel": true, "id": idx.to_string(), "name": "幻灯片",
            "requestId": rid, "conversationId": cid, "userId": uid
        }),
        "expert" | "team" | "lighthouse" => {
            let expert_type = if kind == "team" { "team" } else { "agent" };
            let (expert_id, name) = match kind {
                "lighthouse" => ("ex_2cvvUZQhDyeJ", "腾讯轻量云专家"),
                "team" => ("CloudOpsTeam", "运维专家团队"),
                _ => ("ContentCreator", "内容创作专家"),
            };
            json!({
                "eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
                "mode": "CLOUD", "id": expert_id, "name": name, "expertTitle": name,
                "type": "02-Engineering", "expertType": expert_type, "source": "builtin",
                "version": "1.0.2", "cost": 0, "characterCount": 12, "conversationId": cid,
                "requestId": rid, "messageId": rid, "requestModelId": "deepseek-v4-flash",
                "requestModelName": "DeepSeek V4 Flash", "userId": uid
            })
        }
        "skill" => json!({
            "eventCode": "skill_info", "timestamp": now, "reportDelay": 0,
            "skillId": "skill_2096525080079265792", "name": "pptx", "userId": uid
        }),
        "automation" => json!({
            "eventCode": "automated_task_create_suc", "timestamp": now, "reportDelay": 0,
            "name": "每周工作整理", "type": "cron", "source": "manually",
            "modelId": "deepseek-v4-flash", "modelIsThinking": false,
            "conversationId": cid, "requestId": rid,
            "schedule": {"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"},
            "prompt": "每周五自动整理本周工作", "userId": uid
        }),
        "playbook" => json!({
            "eventCode": "playbook_prompt_send", "timestamp": now, "reportDelay": 0,
            "id": "worker-ledger-freedom-dashboard", "name": "打工人小账本",
            "type": "other", "promptLength": 10, "isOfficial": 1, "source": "discover",
            "conversationId": cid, "requestId": rid, "userId": uid
        }),
        "skin" => json!({
            "eventCode": "appearance_skin_apply", "timestamp": now, "reportDelay": 0,
            "action": "apply", "source": "settings_close", "id": "theme-tkmw7j",
            "vipLevel": "free", "series": "craft", "type": "unknown",
            "name": "和平精英激战金秋", "userId": uid
        }),
        "chat" | "glmchat" | "cat" => {
            let (model_id, model_name) = if kind == "chat" {
                ("deepseek-v4-flash", "DeepSeek V4 Flash")
            } else {
                ("glm-5.2", "GLM-5.2")
            };
            let mode = if kind == "cat" { "night" } else { "craft" };
            json!({
                "eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
                "mode": mode, "conversationId": cid, "requestId": rid, "inputLength": 12,
                "requestModelId": model_id, "requestModelName": model_name,
                "isPlan": false, "agentName": "default", "agentType": "conversation",
                "userId": uid
            })
        }
        _ => json!({ "eventCode": "heartbeat", "timestamp": now, "userId": uid }),
    }
}

// ── 客户端 ────────────────────────────────────────────────────
pub struct ActivityClient {
    client: reqwest::Client,
}

impl ActivityClient {
    pub fn new() -> Result<Self, String> {
        reqwest::Client::builder()
            .timeout(REQUEST_TIMEOUT)
            .connect_timeout(Duration::from_secs(6))
            .build()
            .map(|client| Self { client })
            .map_err(|error| format!("创建活动请求客户端失败: {}", error))
    }

    async fn get(&self, account: &WorkbuddyAccount, url: &str) -> Result<Value, String> {
        let realm = resolve_realm(account.domain.as_deref());
        let mut request = self.client.get(url);
        for (name, value) in outbound_headers(account, realm) {
            request = request.header(name, value);
        }
        let response = request
            .send()
            .await
            .map_err(|error| format!("请求失败: {}", error))?;
        let status = response.status();
        let body = response.text().await.unwrap_or_default();
        let payload: Value = serde_json::from_str(&body).map_err(|_| {
            format!("响应无法解析 (HTTP {}): {}", status.as_u16(), truncate(&body, 200))
        })?;
        Ok(payload)
    }

    async fn post(
        &self,
        account: &WorkbuddyAccount,
        url: &str,
        body: Option<Value>,
    ) -> Result<(u16, Value), String> {
        let realm = resolve_realm(account.domain.as_deref());
        let mut request = self.client.post(url);
        for (name, value) in outbound_headers(account, realm) {
            request = request.header(name, value);
        }
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request
            .send()
            .await
            .map_err(|error| format!("请求失败: {}", error))?;
        let status = response.status().as_u16();
        let text = response.text().await.unwrap_or_default();
        let payload: Value = serde_json::from_str(&text).unwrap_or(Value::Null);
        Ok((status, payload))
    }

    /// 成长任务清单。
    pub async fn fetch_tasks(&self, account: &WorkbuddyAccount) -> Result<Vec<GrowthTask>, String> {
        let realm = resolve_realm(account.domain.as_deref());
        let url = format!("{}/v2/activity/growth/tasks", realm.chat_base());
        let payload = self.get(account, &url).await?;
        if payload.get("code").and_then(Value::as_i64).unwrap_or(-1) != 0 {
            return Err(format!(
                "任务接口返回: {}",
                payload
                    .get("msg")
                    .and_then(Value::as_str)
                    .unwrap_or("未知错误")
            ));
        }
        let raw_tasks = payload
            .get("data")
            .and_then(|data| data.get("tasks"))
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        Ok(raw_tasks
            .iter()
            .filter_map(|task| {
                let code = task
                    .get("task_code")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string();
                if code.is_empty() {
                    return None;
                }
                let spec = spec_for(&code);
                let progress = task.get("progress");
                let current = progress
                    .and_then(|value| value.get("current"))
                    .and_then(Value::as_i64)
                    .unwrap_or(0);
                let target = progress
                    .and_then(|value| value.get("target"))
                    .and_then(Value::as_i64)
                    .or_else(|| spec.map(|spec| spec.target))
                    .unwrap_or(1);
                Some(GrowthTask {
                    name: task
                        .get("title")
                        .and_then(Value::as_str)
                        .map(str::to_string)
                        .or_else(|| spec.map(|spec| spec.name.to_string()))
                        .unwrap_or_else(|| code.clone()),
                    description: task
                        .get("description")
                        .and_then(Value::as_str)
                        .or_else(|| task.get("task_desc").and_then(Value::as_str))
                        .unwrap_or_default()
                        .to_string(),
                    status: task
                        .get("accept_status")
                        .and_then(Value::as_str)
                        .unwrap_or("not_accepted")
                        .to_string(),
                    reward_credit: task
                        .get("reward_credit")
                        .and_then(Value::as_i64)
                        .or_else(|| spec.map(|spec| spec.reward))
                        .unwrap_or(0),
                    reward_energy: task
                        .get("reward_energy")
                        .and_then(Value::as_i64)
                        .unwrap_or(0),
                    unforgeable: spec.map(|spec| spec.unforgeable).unwrap_or(false),
                    reason: spec.map(|spec| spec.reason.to_string()).unwrap_or_default(),
                    current,
                    target,
                    task_code: code,
                })
            })
            .collect())
    }

    pub async fn accept_tasks(
        &self,
        account: &WorkbuddyAccount,
        codes: &[String],
    ) -> Result<bool, String> {
        if codes.is_empty() {
            return Ok(true);
        }
        let realm = resolve_realm(account.domain.as_deref());
        let url = format!("{}/v2/activity/growth/tasks/accept", realm.chat_base());
        let (_, payload) = self
            .post(account, &url, Some(json!({ "task_codes": codes })))
            .await?;
        Ok(payload.get("code").and_then(Value::as_i64).unwrap_or(-1) == 0)
    }

    /// 领奖。desktop 域 400 时降级到网页域（对齐上游实现）。
    pub async fn claim_task(
        &self,
        account: &WorkbuddyAccount,
        code: &str,
    ) -> Result<(bool, i64, i64), String> {
        let realm = resolve_realm(account.domain.as_deref());
        let url = format!(
            "{}/activity/growth/tasks/{}/claim",
            realm.chat_base(),
            code
        );
        let (status, payload) = self.post(account, &url, None).await?;
        if payload.get("code").and_then(Value::as_i64).unwrap_or(-1) == 0 {
            let data = payload.get("data").cloned().unwrap_or(Value::Null);
            return Ok((
                true,
                data.get("credit").and_then(Value::as_i64).unwrap_or(0),
                data.get("energy").and_then(Value::as_i64).unwrap_or(0),
            ));
        }
        if status == 400 {
            let web_url = format!("{}/activity/growth/tasks/{}/claim", CN_WEB_BASE, code);
            let mut request = self.client.post(&web_url);
            for (name, value) in web_claim_headers(account) {
                request = request.header(name, value);
            }
            if let Ok(response) = request.send().await {
                let text = response.text().await.unwrap_or_default();
                if let Ok(payload) = serde_json::from_str::<Value>(&text) {
                    if payload.get("code").and_then(Value::as_i64).unwrap_or(-1) == 0 {
                        let data = payload.get("data").cloned().unwrap_or(Value::Null);
                        return Ok((
                            true,
                            data.get("credit").and_then(Value::as_i64).unwrap_or(0),
                            data.get("energy").and_then(Value::as_i64).unwrap_or(0),
                        ));
                    }
                }
            }
        }
        Ok((false, 0, 0))
    }

    /// 事件上报（点亮任务）。`billing` 为 true 时打到账单域。
    pub async fn report_events(
        &self,
        account: &WorkbuddyAccount,
        events: &[Value],
        billing: bool,
    ) -> Result<bool, String> {
        let realm = resolve_realm(account.domain.as_deref());
        let base = if billing {
            realm.bill_base()
        } else {
            realm.chat_base()
        };
        let url = format!("{}/v2/report", base);
        let (_, payload) = self
            .post(account, &url, Some(Value::Array(events.to_vec())))
            .await?;
        Ok(payload.get("code").and_then(Value::as_i64).unwrap_or(-1) == 0)
    }

    pub async fn energy(&self, account: &WorkbuddyAccount) -> Option<i64> {
        let realm = resolve_realm(account.domain.as_deref());
        let url = format!("{}/v2/activity/growth/energy", realm.chat_base());
        self.get(account, &url)
            .await
            .ok()
            .and_then(|payload| payload.get("data").cloned())
            .and_then(|data| data.get("balance").and_then(Value::as_i64))
    }

    pub async fn streak_days(&self, account: &WorkbuddyAccount) -> Option<i64> {
        let realm = resolve_realm(account.domain.as_deref());
        let url = format!("{}/activity/growth/streak", realm.chat_base());
        self.get(account, &url)
            .await
            .ok()
            .and_then(|payload| payload.get("data").cloned())
            .and_then(|data| data.get("streak").cloned())
            .and_then(|streak| streak.get("days").and_then(Value::as_i64))
    }

    pub async fn travel_status(&self, account: &WorkbuddyAccount) -> Result<TravelStatus, String> {
        let realm = resolve_realm(account.domain.as_deref());
        let url = format!("{}/activity/growth/buddy/travel/status", realm.chat_base());
        let payload = self.get(account, &url).await?;
        let data = payload.get("data").cloned().unwrap_or(Value::Null);
        Ok(TravelStatus {
            state: data
                .get("state")
                .and_then(Value::as_str)
                .unwrap_or("unknown")
                .to_string(),
            daily_limit_reached: data
                .get("daily_limit_reached")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            raw: data,
        })
    }

    /// 猫猫旅行闭环：arrived → 领奖；idle 且未达日限 → 派出。
    pub async fn cat_travel(&self, account: &WorkbuddyAccount) -> ActivityRunLog {
        let realm = resolve_realm(account.domain.as_deref());
        let uid = account.uid.clone().unwrap_or_default();
        let mut logs = Vec::new();
        let label = account_label(account);
        let status = match self.travel_status(account).await {
            Ok(status) => status,
            Err(error) => {
                return ActivityRunLog {
                    ok: false,
                    account_id: account.id.clone(),
                    label,
                    earned_credit: 0,
                    logs: vec![format!("查询旅行状态失败: {}", error)],
                }
            }
        };
        let mut earned = 0i64;
        match status.state.as_str() {
            "arrived" => {
                let url = format!("{}/activity/growth/buddy/travel/claim", realm.chat_base());
                match self.post(account, &url, None).await {
                    Ok((_, payload)) if payload.get("code").and_then(Value::as_i64) == Some(0) => {
                        let credit = payload
                            .get("data")
                            .and_then(|data| data.get("reward_credit"))
                            .and_then(Value::as_i64)
                            .unwrap_or(0);
                        earned += credit;
                        logs.push(format!("旅行归来领奖成功！获得 {} 积分", credit));
                    }
                    Ok((status, payload)) => logs.push(format!(
                        "领奖失败 (HTTP {}): {}",
                        status,
                        payload.get("msg").and_then(Value::as_str).unwrap_or("")
                    )),
                    Err(error) => logs.push(format!("领奖失败: {}", error)),
                }
            }
            "idle" => {
                if status.daily_limit_reached {
                    logs.push("猫猫今日已完成旅行，明日 00:00 刷新".to_string());
                } else {
                    let url = format!("{}/activity/growth/buddy/travel/depart", realm.chat_base());
                    match self.post(account, &url, None).await {
                        Ok((_, payload))
                            if payload.get("code").and_then(Value::as_i64) == Some(0) =>
                        {
                            logs.push("猫猫已成功派出旅行，预计数小时后归来！".to_string())
                        }
                        Ok((status, payload)) => logs.push(format!(
                            "派出旅行失败 (HTTP {}): {}",
                            status,
                            payload.get("msg").and_then(Value::as_str).unwrap_or("")
                        )),
                        Err(error) => logs.push(format!("派出旅行失败: {}", error)),
                    }
                }
            }
            "traveling" => logs.push("猫猫正在旅行途中，请稍后再来查看！".to_string()),
            other => logs.push(format!("当前状态: {}", other)),
        }
        if !uid.is_empty() {
            let _ = self
                .report_events(
                    account,
                    &[build_event(&uid, "heartbeat", 0)],
                    false,
                )
                .await;
        }
        ActivityRunLog {
            ok: true,
            account_id: account.id.clone(),
            label,
            earned_credit: earned,
            logs,
        }
    }

    /// 完整执行成长任务：接取 → 上报点亮 → 领奖 → 顺手猫猫。
    pub async fn run_growth_tasks(&self, account: &WorkbuddyAccount) -> ActivityRunLog {
        let realm = resolve_realm(account.domain.as_deref());
        let label = account_label(account);
        let mut logs = Vec::new();
        let mut earned = 0i64;
        if realm != WorkbuddyRealm::Cn {
            return ActivityRunLog {
                ok: false,
                account_id: account.id.clone(),
                label,
                earned_credit: 0,
                logs: vec!["国际版不适用国内成长任务中心".to_string()],
            };
        }
        let uid = account.uid.clone().unwrap_or_default();
        logs.push(format!("开始为账号 {} 运行成长任务自动化...", label));

        let mut tasks = match self.fetch_tasks(account).await {
            Ok(tasks) => tasks,
            Err(error) => {
                logs.push(format!("获取任务清单失败: {}", error));
                return ActivityRunLog {
                    ok: false,
                    account_id: account.id.clone(),
                    label,
                    earned_credit: 0,
                    logs,
                };
            }
        };
        if tasks.is_empty() {
            logs.push("未能获取到任务清单，请检查网络或账号状态".to_string());
            return ActivityRunLog {
                ok: false,
                account_id: account.id.clone(),
                label,
                earned_credit: 0,
                logs,
            };
        }

        let unaccepted: Vec<String> = tasks
            .iter()
            .filter(|task| task.status == "not_accepted" && !task.unforgeable)
            .map(|task| task.task_code.clone())
            .collect();
        if !unaccepted.is_empty() {
            logs.push(format!(
                "发现 {} 个待接取任务，正在批量接取...",
                unaccepted.len()
            ));
            let _ = self.accept_tasks(account, &unaccepted).await;
            tokio::time::sleep(MIN_REQUEST_GAP).await;
            if let Ok(refreshed) = self.fetch_tasks(account).await {
                tasks = refreshed;
            }
        }

        for task in &tasks {
            let Some(spec) = spec_for(&task.task_code) else {
                continue;
            };
            if spec.unforgeable || task.status == "claimed" {
                continue;
            }
            if task.status == "completed" || task.current >= task.target {
                match self.claim_task(account, &task.task_code).await {
                    Ok((true, credit, _)) => {
                        earned += credit;
                        logs.push(format!("✓ 任务 [{}] 领奖成功: +{} 积分", spec.name, credit));
                    }
                    _ => logs.push(format!("! 任务 [{}] 领奖失败", spec.name)),
                }
                tokio::time::sleep(MIN_REQUEST_GAP).await;
                continue;
            }

            let need = (task.target - task.current).max(1);
            logs.push(format!("正在点亮任务 [{}] (需上报 {} 次)...", spec.name, need));
            for index in 0..need {
                let event = build_event(&uid, spec.kind, index as usize);
                let _ = self.report_events(account, &[event], true).await;
                if index + 1 < need {
                    tokio::time::sleep(MIN_REQUEST_GAP).await;
                }
            }
            tokio::time::sleep(Duration::from_millis(1500)).await;
            match self.claim_task(account, &task.task_code).await {
                Ok((true, credit, _)) => {
                    earned += credit;
                    logs.push(format!(
                        "✓ 任务 [{}] 点亮并领奖成功: +{} 积分",
                        spec.name, credit
                    ));
                }
                _ => logs.push(format!("? 任务 [{}] 已上报点亮，领奖稍后结算", spec.name)),
            }
            tokio::time::sleep(MIN_REQUEST_GAP).await;
        }

        let travel = self.cat_travel(account).await;
        logs.extend(travel.logs);
        earned += travel.earned_credit;

        logger::log_info(&format!(
            "[WorkbuddyActivity] 成长任务完成 account={} earned={}",
            account.id, earned
        ));
        ActivityRunLog {
            ok: true,
            account_id: account.id.clone(),
            label,
            earned_credit: earned,
            logs,
        }
    }

    /// 面板用：一次拉齐能量 / 连续打卡 / 猫猫状态 / 任务清单。
    pub async fn overview(&self, account: &WorkbuddyAccount) -> ActivityOverview {
        let realm = resolve_realm(account.domain.as_deref());
        let energy = self.energy(account).await.unwrap_or(0);
        let streak_days = self.streak_days(account).await.unwrap_or(0);
        let travel = self
            .travel_status(account)
            .await
            .unwrap_or(TravelStatus {
                state: "unknown".to_string(),
                daily_limit_reached: false,
                raw: Value::Null,
            });
        let (tasks, message) = if realm == WorkbuddyRealm::Cn {
            match self.fetch_tasks(account).await {
                Ok(tasks) => (tasks, String::new()),
                Err(error) => (Vec::new(), error),
            }
        } else {
            (Vec::new(), "国际版不适用国内成长任务中心".to_string())
        };
        ActivityOverview {
            ok: message.is_empty(),
            realm,
            energy,
            streak_days,
            travel,
            tasks,
            message,
        }
    }
}

fn account_label(account: &WorkbuddyAccount) -> String {
    account
        .nickname
        .clone()
        .filter(|value| !value.trim().is_empty())
        .unwrap_or_else(|| account.email.clone())
}

fn truncate(value: &str, limit: usize) -> String {
    if value.chars().count() <= limit {
        return value.to_string();
    }
    value.chars().take(limit).collect()
}

/// 便捷入口：按账号 id 执行成长任务自动化。
pub async fn run_growth_tasks_for_account(account_id: &str) -> Result<ActivityRunLog, String> {
    let account = workbuddy_account::load_account(account_id)
        .ok_or_else(|| format!("账号不存在: {}", account_id))?;
    let client = ActivityClient::new()?;
    Ok(client.run_growth_tasks(&account).await)
}

/// 便捷入口：按账号 id 执行猫猫旅行闭环。
pub async fn cat_travel_for_account(account_id: &str) -> Result<ActivityRunLog, String> {
    let account = workbuddy_account::load_account(account_id)
        .ok_or_else(|| format!("账号不存在: {}", account_id))?;
    let client = ActivityClient::new()?;
    Ok(client.cat_travel(&account).await)
}

/// 便捷入口：账号活动总览。
pub async fn overview_for_account(account_id: &str) -> Result<ActivityOverview, String> {
    let account = workbuddy_account::load_account(account_id)
        .ok_or_else(|| format!("账号不存在: {}", account_id))?;
    let client = ActivityClient::new()?;
    Ok(client.overview(&account).await)
}

impl ActivityClient {
    /// 仅执行指定 task_code 的点亮 + 领奖（调度器夜猫子等场景用）。
    pub async fn run_single_task(
        &self,
        account: &WorkbuddyAccount,
        task_code: &str,
    ) -> ActivityRunLog {
        let realm = resolve_realm(account.domain.as_deref());
        let label = account_label(account);
        let mut logs = Vec::new();
        let mut earned = 0i64;
        if realm != WorkbuddyRealm::Cn {
            return ActivityRunLog {
                ok: false,
                account_id: account.id.clone(),
                label,
                earned_credit: 0,
                logs: vec!["国际版不适用国内成长任务中心".to_string()],
            };
        }
        let Some(spec) = spec_for(task_code) else {
            return ActivityRunLog {
                ok: false,
                account_id: account.id.clone(),
                label,
                earned_credit: 0,
                logs: vec![format!("未知任务: {}", task_code)],
            };
        };
        if spec.unforgeable {
            return ActivityRunLog {
                ok: false,
                account_id: account.id.clone(),
                label,
                earned_credit: 0,
                logs: vec![format!("任务 [{}] 无法伪造，已跳过", spec.name)],
            };
        }
        let uid = account.uid.clone().unwrap_or_default();
        logs.push(format!("执行任务 [{}]...", spec.name));

        let tasks = match self.fetch_tasks(account).await {
            Ok(tasks) => tasks,
            Err(error) => {
                logs.push(format!("获取任务清单失败: {}", error));
                return ActivityRunLog {
                    ok: false,
                    account_id: account.id.clone(),
                    label,
                    earned_credit: 0,
                    logs,
                };
            }
        };
        let task = tasks.iter().find(|task| task.task_code == task_code);
        let Some(task) = task else {
            logs.push(format!("任务清单中不存在 [{}]", spec.name));
            return ActivityRunLog {
                ok: false,
                account_id: account.id.clone(),
                label,
                earned_credit: 0,
                logs,
            };
        };
        if task.status == "claimed" {
            logs.push(format!("任务 [{}] 今日已领取", spec.name));
            return ActivityRunLog {
                ok: true,
                account_id: account.id.clone(),
                label,
                earned_credit: 0,
                logs,
            };
        }
        if task.status == "not_accepted" {
            let _ = self.accept_tasks(account, &[task_code.to_string()]).await;
            tokio::time::sleep(MIN_REQUEST_GAP).await;
        }
        if task.status != "completed" && task.current < task.target {
            let need = (task.target - task.current).max(1);
            for index in 0..need {
                let event = build_event(&uid, spec.kind, index as usize);
                let _ = self.report_events(account, &[event], true).await;
                if index + 1 < need {
                    tokio::time::sleep(MIN_REQUEST_GAP).await;
                }
            }
            tokio::time::sleep(Duration::from_millis(1200)).await;
        }
        match self.claim_task(account, task_code).await {
            Ok((true, credit, _)) => {
                earned += credit;
                logs.push(format!("✓ 任务 [{}] 领奖成功: +{} 积分", spec.name, credit));
            }
            Ok((false, _, _)) => logs.push(format!("? 任务 [{}] 已上报，领奖稍后结算", spec.name)),
            Err(error) => logs.push(format!("! 任务 [{}] 领奖失败: {}", spec.name, error)),
        }
        ActivityRunLog {
            ok: true,
            account_id: account.id.clone(),
            label,
            earned_credit: earned,
            logs,
        }
    }

    /// 活跃上报：连发对话事件，点亮连登/对话类任务。
    pub async fn run_activity_report(&self, account: &WorkbuddyAccount) -> ActivityRunLog {
        let realm = resolve_realm(account.domain.as_deref());
        let label = account_label(account);
        let mut logs = Vec::new();
        if realm != WorkbuddyRealm::Cn {
            return ActivityRunLog {
                ok: false,
                account_id: account.id.clone(),
                label,
                earned_credit: 0,
                logs: vec!["国际版不适用国内活跃上报".to_string()],
            };
        }
        let uid = account.uid.clone().unwrap_or_default();
        if uid.trim().is_empty() {
            return ActivityRunLog {
                ok: false,
                account_id: account.id.clone(),
                label,
                earned_credit: 0,
                logs: vec!["账号缺少 uid，无法构造事件".to_string()],
            };
        }
        logs.push("开始活跃上报（对话事件 x5）...".to_string());
        for index in 0..5 {
            let event = build_event(&uid, "chat", index);
            match self.report_events(account, &[event], true).await {
                Ok(true) => logs.push(format!("✓ 对话事件 {}/5 上报成功", index + 1)),
                Ok(false) => logs.push(format!("! 对话事件 {}/5 上报被拒", index + 1)),
                Err(error) => logs.push(format!("! 对话事件 {}/5 上报失败: {}", index + 1, error)),
            }
            if index < 4 {
                tokio::time::sleep(MIN_REQUEST_GAP).await;
            }
        }
        ActivityRunLog {
            ok: true,
            account_id: account.id.clone(),
            label,
            earned_credit: 0,
            logs,
        }
    }
}

/// 便捷入口：活跃上报。
pub async fn run_activity_report_for_account(account_id: &str) -> Result<ActivityRunLog, String> {
    let account = workbuddy_account::load_account(account_id)
        .ok_or_else(|| format!("账号不存在: {}", account_id))?;
    let client = ActivityClient::new()?;
    Ok(client.run_activity_report(&account).await)
}

/// 便捷入口：夜猫子（仅 black_cat）。
pub async fn run_night_cat_for_account(account_id: &str) -> Result<ActivityRunLog, String> {
    let account = workbuddy_account::load_account(account_id)
        .ok_or_else(|| format!("账号不存在: {}", account_id))?;
    let client = ActivityClient::new()?;
    Ok(client.run_single_task(&account, "black_cat").await)
}

/// 便捷入口：按 task_code 执行单个任务。
pub async fn run_task_code_for_account(
    account_id: &str,
    task_code: &str,
) -> Result<ActivityRunLog, String> {
    let account = workbuddy_account::load_account(account_id)
        .ok_or_else(|| format!("账号不存在: {}", account_id))?;
    let client = ActivityClient::new()?;
    Ok(client.run_single_task(&account, task_code).await)
}

/// 是否国内版账号（批量执行时过滤）。
pub fn is_cn_account(account: &WorkbuddyAccount) -> bool {
    resolve_realm(account.domain.as_deref()) == WorkbuddyRealm::Cn
}

fn fail_log(account: &WorkbuddyAccount, message: String) -> ActivityRunLog {
    ActivityRunLog {
        ok: false,
        account_id: account.id.clone(),
        label: account_label(account),
        earned_credit: 0,
        logs: vec![message],
    }
}

/// 对单个账号执行指定活动类型。
pub async fn run_kind_for_account(kind: &str, account_id: &str) -> Result<ActivityRunLog, String> {
    match kind {
        "growth" | "schoolSeason" => run_growth_tasks_for_account(account_id).await,
        "catTravel" | "cat" => cat_travel_for_account(account_id).await,
        "nightCat" => run_night_cat_for_account(account_id).await,
        "activityReport" | "activity_report" => run_activity_report_for_account(account_id).await,
        "checkin" => {
            let account = workbuddy_account::load_account(account_id)
                .ok_or_else(|| format!("账号不存在: {}", account_id))?;
            let label = account_label(&account);
            if !is_cn_account(&account) {
                return Ok(ActivityRunLog {
                    ok: false,
                    account_id: account.id.clone(),
                    label,
                    earned_credit: 0,
                    logs: vec!["国际版不适用国内签到".to_string()],
                });
            }
            match crate::modules::codebuddy_cn_oauth::perform_checkin(
                &account.access_token,
                account.uid.as_deref(),
                account.enterprise_id.as_deref(),
                account.domain.as_deref(),
            )
            .await
            {
                Ok(response) => {
                    if response.success {
                        let now = chrono::Utc::now().timestamp();
                        let streak = response
                            .streak_days
                            .and_then(|value| i32::try_from(value).ok())
                            .unwrap_or_else(|| account.checkin_streak.unwrap_or(0).saturating_add(1));
                        let reward = response.reward.clone().or_else(|| {
                            response
                                .credit
                                .map(|credit| serde_json::json!({ "credit": credit }))
                        });
                        let _ = workbuddy_account::update_checkin_info(
                            account_id,
                            Some(now),
                            streak,
                            reward,
                        );
                        Ok(ActivityRunLog {
                            ok: true,
                            account_id: account.id.clone(),
                            label,
                            earned_credit: response.credit.unwrap_or(0),
                            logs: vec![format!(
                                "✓ 签到成功: +{} 积分",
                                response.credit.unwrap_or(0)
                            )],
                        })
                    } else {
                        Ok(fail_log(
                            &account,
                            format!(
                                "签到未成功: {}",
                                response.message.unwrap_or_else(|| "未知原因".into())
                            ),
                        ))
                    }
                }
                Err(error) => Ok(fail_log(&account, format!("签到失败: {}", error))),
            }
        }
        other => Err(format!("未知活动类型: {}", other)),
    }
}

/// 全账号批量执行：仅国内版，账号间强制 >=1s 间隔。
pub async fn run_kind_for_all_accounts(kind: &str) -> Result<Vec<ActivityRunLog>, String> {
    let accounts = workbuddy_account::list_accounts();
    let mut results = Vec::new();
    for account in accounts {
        if !is_cn_account(&account) {
            continue;
        }
        let result = run_kind_for_account(kind, &account.id).await.unwrap_or_else(|err| {
            fail_log(&account, err)
        });
        results.push(result);
        tokio::time::sleep(MIN_REQUEST_GAP).await;
    }
    Ok(results)
}

/// 一键日常：签到 → 猫猫 → 活跃上报 → 成长任务；夜窗内再跑夜猫子。
/// 每账号每类写一条活动日志（source=manual）。
pub async fn run_daily_all_accounts() -> Result<Vec<ActivityRunLog>, String> {
    use crate::modules::workbuddy_activity_log::{self as activity_log, ActivityLogKind, ActivityLogSource};

    let accounts = workbuddy_account::list_accounts();
    let mut results = Vec::new();
    let hour = chrono::Local::now().hour();
    let in_night_window = matches!(hour, 23 | 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7);
    let steps: &[(&str, ActivityLogKind)] = if in_night_window {
        &[
            ("checkin", ActivityLogKind::Checkin),
            ("catTravel", ActivityLogKind::CatTravel),
            ("activityReport", ActivityLogKind::ActivityReport),
            ("growth", ActivityLogKind::Growth),
            ("nightCat", ActivityLogKind::NightCat),
        ]
    } else {
        &[
            ("checkin", ActivityLogKind::Checkin),
            ("catTravel", ActivityLogKind::CatTravel),
            ("activityReport", ActivityLogKind::ActivityReport),
            ("growth", ActivityLogKind::Growth),
        ]
    };
    for account in accounts {
        if !is_cn_account(&account) {
            continue;
        }
        for (kind_str, log_kind) in steps {
            let result = run_kind_for_account(kind_str, &account.id)
                .await
                .unwrap_or_else(|err| fail_log(&account, err));
            activity_log::append_activity_log(
                ActivityLogSource::Manual,
                *log_kind,
                result.account_id.clone(),
                result.label.clone(),
                result.ok,
                result.earned_credit,
                result
                    .logs
                    .first()
                    .cloned()
                    .unwrap_or_else(|| log_kind.label().to_string()),
                result.logs.clone(),
            );
            results.push(result);
            tokio::time::sleep(MIN_REQUEST_GAP).await;
        }
    }
    Ok(results)
}

/// 全账号总览；`use_cache=true` 时优先读 5 分钟内缓存。
pub async fn overview_all_accounts(
    use_cache: bool,
    force_refresh: bool,
) -> Result<Vec<(String, ActivityOverview)>, String> {
    let accounts = workbuddy_account::list_accounts();
    let mut out = Vec::new();
    for account in accounts {
        if !is_cn_account(&account) {
            continue;
        }
        if use_cache && !force_refresh {
            if let Ok(Some(cached)) =
                crate::modules::workbuddy_activity_cache::get_cached_overview(&account.id, None)
            {
                out.push((account.id.clone(), cached));
                continue;
            }
        }
        match overview_for_account(&account.id).await {
            Ok(overview) => {
                let _ =
                    crate::modules::workbuddy_activity_cache::put_overview(&account.id, overview.clone());
                out.push((account.id.clone(), overview));
            }
            Err(error) => {
                out.push((
                    account.id.clone(),
                    ActivityOverview {
                        ok: false,
                        realm: resolve_realm(account.domain.as_deref()),
                        energy: 0,
                        streak_days: 0,
                        travel: TravelStatus {
                            state: "unknown".to_string(),
                            daily_limit_reached: false,
                            raw: Value::Null,
                        },
                        tasks: Vec::new(),
                        message: error,
                    },
                ));
            }
        }
        tokio::time::sleep(MIN_REQUEST_GAP).await;
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn derive_id_is_stable_and_isolated_per_uid() {
        let a1 = derive_id("uid-1", "machine");
        let a2 = derive_id("uid-1", "machine");
        let b = derive_id("uid-2", "machine");
        assert_eq!(a1, a2, "同一账号必须稳定派生同一机器码");
        assert_ne!(a1, b, "不同账号必须隔离");
        assert_eq!(a1.len(), 32);
    }

    #[test]
    fn derive_id_handles_empty_uid() {
        assert_eq!(derive_id("", "machine"), derive_id("anonymous", "machine"));
    }

    #[test]
    fn request_id_prefix_is_stable() {
        let first = generate_request_id("uid-9");
        let second = generate_request_id("uid-9");
        let prefix = derive_id("uid-9", "req");
        assert!(first.starts_with(&prefix));
        assert!(second.starts_with(&prefix));
        assert!(first.len() > prefix.len());
    }

    #[test]
    fn realm_detection_follows_account_domain() {
        assert_eq!(resolve_realm(Some("www.workbuddy.cn")), WorkbuddyRealm::Cn);
        assert_eq!(resolve_realm(Some("copilot.tencent.com")), WorkbuddyRealm::Cn);
        assert_eq!(resolve_realm(Some("www.workbuddy.ai")), WorkbuddyRealm::Global);
        assert_eq!(resolve_realm(None), WorkbuddyRealm::Cn);
    }

    #[test]
    fn fingerprint_uses_stable_ids() {
        let fp = desktop_fingerprint("uid-7", "tester");
        assert_eq!(fp["machineId"], json!(machine_id("uid-7")));
        assert_eq!(fp["sessionId"], json!(session_id("uid-7")));
        assert_eq!(fp["ideName"], json!("WorkBuddy"));
        assert_eq!(fp["userId"], json!("uid-7"));
    }

    #[test]
    fn every_task_spec_has_a_builder_kind() {
        for spec in TASK_SPECS {
            if spec.unforgeable {
                continue;
            }
            let event = build_event("uid", spec.kind, 0);
            assert!(
                event.get("eventCode").is_some(),
                "任务 {} 缺少事件构造",
                spec.code
            );
            assert_eq!(kind_for(spec.code), Some(spec.kind));
        }
    }

    #[test]
    fn events_carry_uid_and_ids() {
        let event = build_event("uid-42", "canvas", 3);
        assert_eq!(event["userId"], json!("uid-42"));
        assert_eq!(event["eventCode"], json!("wbx_design_canvas_task_create"));
        assert!(event["conversationId"].as_str().unwrap().contains("wb-task-"));
    }

    #[test]
    fn night_cat_event_uses_night_mode() {
        let event = build_event("uid", "cat", 0);
        assert_eq!(event["mode"], json!("night"));
        assert_eq!(event["requestModelId"], json!("glm-5.2"));
    }
}
