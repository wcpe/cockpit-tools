//! CodeBuddy / CodeBuddy CN / WorkBuddy local API service.
//!
//! This mirrors the Codex API Service pattern: Cockpit materializes a manifest
//! for the bundled `cockpit-cliproxy` sidecar and starts it on a loopback (or
//! LAN) port. The sidecar speaks the CodeBuddy wire protocol
//! (`POST /v2/chat/completions` with a JWT bearer token) and exposes an
//! OpenAI-compatible `/v1` surface to third-party clients.
//!
//! Cockpit remains the token authority: it materializes account JWTs into the
//! manifest and persists any token the sidecar rotates after a 401, so the
//! account store never drifts from the live credential.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine as _;
use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::{Child, Command as TokioCommand};
use tokio::sync::Mutex;

use crate::models::codebuddy_local_access::{
    CodebuddyFetchModelsAccountResult, CodebuddyFetchModelsResult, CodebuddyLocalAccessAccountOption,
    CodebuddyLocalAccessAccountRef, CodebuddyLocalAccessCollection, CodebuddyLocalAccessPlatform,
    CodebuddyLocalAccessScope, CodebuddyLocalAccessState, CodebuddyLocalAccessTestResult,
    CodebuddyModelInfo, CodebuddyProbeResult, CODEBUDDY_DEFAULT_MODEL_IDS,
};
use crate::modules::codex_local_access::{sanitize_sidecar_command_env, sidecar_binary_path};
use crate::modules::{
    account, atomic_write, codebuddy_account, codebuddy_cn_account, logger, workbuddy_account,
};

const STATE_FILE: &str = "codebuddy_local_access.json";
const RUNTIME_DIR: &str = "codebuddy_local_access_sidecar";
const SIDECAR_CONFIG_FILE: &str = "config.json";
const SIDECAR_MANIFEST_FILE: &str = "manifest.json";
const SIDECAR_AUTHS_DIR: &str = "auths";
pub const SIDECAR_API_KEY_ID: &str = "codebuddy_api_service";
const LOCALHOST_BIND_HOST: &str = "127.0.0.1";
const LAN_BIND_HOST: &str = "0.0.0.0";
const CLIENT_URL_HOST: &str = "127.0.0.1";
const SIDECAR_READY_TIMEOUT: Duration = Duration::from_secs(20);
/// Refresh a selected account when its JWT expires within this window.
const TOKEN_REFRESH_WINDOW_MS: i64 = 6 * 60 * 60 * 1000;

#[derive(Default)]
struct ServiceRuntime {
    running: bool,
    port: Option<u16>,
    bind_host: Option<String>,
    last_error: Option<String>,
    child: Option<Child>,
}

impl ServiceRuntime {
    fn base_url(&self) -> Option<String> {
        self.port
            .map(|port| format!("http://{}:{}", CLIENT_URL_HOST, port))
    }
}

fn runtime() -> &'static Mutex<ServiceRuntime> {
    static RUNTIME: OnceLock<Mutex<ServiceRuntime>> = OnceLock::new();
    RUNTIME.get_or_init(|| Mutex::new(ServiceRuntime::default()))
}

fn lifecycle_lock() -> &'static Mutex<()> {
    static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
    LOCK.get_or_init(|| Mutex::new(()))
}

pub fn state_path() -> Result<PathBuf, String> {
    Ok(account::get_data_dir()?.join(STATE_FILE))
}

fn runtime_dir() -> Result<PathBuf, String> {
    Ok(account::get_data_dir()?.join(RUNTIME_DIR))
}

fn now_ms() -> i64 {
    chrono::Utc::now().timestamp_millis()
}

fn generate_api_key() -> String {
    use rand::Rng;
    let bytes: Vec<u8> = (0..24).map(|_| rand::thread_rng().gen::<u8>()).collect();
    format!(
        "cbk-{}",
        bytes
            .iter()
            .map(|byte| format!("{:02x}", byte))
            .collect::<String>()
    )
}

fn normalize_collection(collection: &mut CodebuddyLocalAccessCollection) {
    if collection.port == 0 {
        collection.port = CodebuddyLocalAccessCollection::default().port;
    }
    collection.api_key = collection.api_key.trim().to_string();
    if collection.api_key.is_empty() {
        collection.api_key = generate_api_key();
    }
    collection.model_ids = normalize_model_ids(&collection.model_ids);
    let mut seen = std::collections::HashSet::new();
    collection.accounts.retain(|entry| {
        !entry.account_id.trim().is_empty()
            && seen.insert((entry.platform.as_str(), entry.account_id.clone()))
    });
}

fn normalize_model_ids(values: &[String]) -> Vec<String> {
    let mut seen = std::collections::HashSet::new();
    let mut out = Vec::new();
    for value in values {
        let trimmed = value.trim();
        if trimmed.is_empty() {
            continue;
        }
        if seen.insert(trimmed.to_lowercase()) {
            out.push(trimmed.to_string());
        }
    }
    out
}

pub fn load_collection() -> CodebuddyLocalAccessCollection {
    let Ok(path) = state_path() else {
        return CodebuddyLocalAccessCollection::default();
    };
    if !path.exists() {
        return CodebuddyLocalAccessCollection::default();
    }
    let Ok(content) = std::fs::read_to_string(&path) else {
        return CodebuddyLocalAccessCollection::default();
    };
    match serde_json::from_str::<CodebuddyLocalAccessCollection>(&content) {
        Ok(mut collection) => {
            normalize_collection(&mut collection);
            collection
        }
        Err(error) => {
            logger::log_codex_api_warn(&format!(
                "[CodebuddyLocalAccess] 读取配置失败，使用默认值: {}",
                error
            ));
            CodebuddyLocalAccessCollection::default()
        }
    }
}

pub fn save_collection(collection: &CodebuddyLocalAccessCollection) -> Result<(), String> {
    let path = state_path()?;
    let content = serde_json::to_string_pretty(collection)
        .map_err(|error| format!("序列化配置失败: {}", error))?;
    atomic_write::write_string_atomic(&path, &content)
}

/// Decode the `exp` claim of a JWT (milliseconds since epoch).
fn jwt_exp_ms(token: &str) -> Option<i64> {
    let payload = token.split('.').nth(1)?;
    let decoded = URL_SAFE_NO_PAD.decode(payload.trim()).ok()?;
    let value: Value = serde_json::from_slice(&decoded).ok()?;
    let exp = value.get("exp")?.as_i64()?;
    if exp <= 0 {
        return None;
    }
    Some(exp.saturating_mul(1000))
}

struct ResolvedAccount {
    label: String,
    uid: Option<String>,
    access_token: String,
    refresh_token: Option<String>,
    expires_at_ms: Option<i64>,
    credits_remain: Option<i64>,
    credits_size: Option<i64>,
    realm: String,
}

fn resolve_account(
    platform: CodebuddyLocalAccessPlatform,
    account_id: &str,
) -> Result<ResolvedAccount, String> {
    let missing = || format!("账号不存在或已删除: {}", account_id);
    let realm_for = |platform: CodebuddyLocalAccessPlatform| -> String {
        match platform {
            CodebuddyLocalAccessPlatform::Workbuddy | CodebuddyLocalAccessPlatform::CodebuddyCn => {
                "cn".to_string()
            }
            CodebuddyLocalAccessPlatform::Codebuddy => "global".to_string(),
        }
    };
    let resolved = match platform {
        CodebuddyLocalAccessPlatform::Workbuddy => {
            let account = workbuddy_account::load_account(account_id).ok_or_else(missing)?;
            let (remain, size) = quota_credits_from_raw(account.quota_raw.as_ref());
            ResolvedAccount {
                label: account.email.clone(),
                uid: account.uid.clone(),
                access_token: account.access_token,
                refresh_token: account.refresh_token,
                expires_at_ms: account.expires_at,
                credits_remain: remain,
                credits_size: size,
                realm: realm_for(platform),
            }
        }
        CodebuddyLocalAccessPlatform::CodebuddyCn => {
            let account = codebuddy_cn_account::load_account(account_id).ok_or_else(missing)?;
            let (remain, size) = quota_credits_from_raw(account.quota_raw.as_ref());
            ResolvedAccount {
                label: account.email.clone(),
                uid: account.uid.clone(),
                access_token: account.access_token,
                refresh_token: account.refresh_token,
                expires_at_ms: account.expires_at,
                credits_remain: remain,
                credits_size: size,
                realm: realm_for(platform),
            }
        }
        CodebuddyLocalAccessPlatform::Codebuddy => {
            let account = codebuddy_account::load_account(account_id).ok_or_else(missing)?;
            let (remain, size) = quota_credits_from_raw(account.quota_raw.as_ref());
            ResolvedAccount {
                label: account.email.clone(),
                uid: account.uid.clone(),
                access_token: account.access_token,
                refresh_token: account.refresh_token,
                expires_at_ms: account.expires_at,
                credits_remain: remain,
                credits_size: size,
                realm: realm_for(platform),
            }
        }
    };
    if resolved.access_token.trim().is_empty() {
        return Err(format!("账号 {} 缺少访问令牌", account_id));
    }
    Ok(resolved)
}

async fn refresh_account(
    platform: CodebuddyLocalAccessPlatform,
    account_id: &str,
) -> Result<(), String> {
    match platform {
        CodebuddyLocalAccessPlatform::Workbuddy => {
            workbuddy_account::refresh_account_token(account_id)
                .await
                .map(|_| ())
        }
        CodebuddyLocalAccessPlatform::CodebuddyCn => {
            codebuddy_cn_account::refresh_account_token(account_id)
                .await
                .map(|_| ())
        }
        CodebuddyLocalAccessPlatform::Codebuddy => {
            codebuddy_account::refresh_account_token(account_id)
                .await
                .map(|_| ())
        }
    }
}

fn persist_rotated_tokens(
    platform: CodebuddyLocalAccessPlatform,
    account_id: &str,
    access_token: &str,
    refresh_token: Option<&str>,
    expires_at_ms: Option<i64>,
) -> Result<(), String> {
    match platform {
        CodebuddyLocalAccessPlatform::Workbuddy => workbuddy_account::persist_refreshed_tokens(
            account_id,
            access_token,
            refresh_token,
            expires_at_ms,
        )
        .map(|_| ()),
        CodebuddyLocalAccessPlatform::CodebuddyCn => {
            codebuddy_cn_account::persist_refreshed_tokens(
                account_id,
                access_token,
                refresh_token,
                expires_at_ms,
            )
            .map(|_| ())
        }
        CodebuddyLocalAccessPlatform::Codebuddy => codebuddy_account::persist_refreshed_tokens(
            account_id,
            access_token,
            refresh_token,
            expires_at_ms,
        )
        .map(|_| ()),
    }
}

/// Refresh selected accounts whose JWT is missing its expiry or is about to
/// expire. A failed refresh keeps the existing token so the service can still
/// start (the upstream error is surfaced on the first request).
async fn refresh_expiring_accounts(collection: &CodebuddyLocalAccessCollection) {
    for entry in &collection.accounts {
        let Ok(resolved) = resolve_account(entry.platform, &entry.account_id) else {
            continue;
        };
        let expiry = resolved
            .expires_at_ms
            .filter(|value| *value > 0)
            .or_else(|| jwt_exp_ms(&resolved.access_token));
        let Some(expiry) = expiry else {
            continue;
        };
        if expiry - now_ms() > TOKEN_REFRESH_WINDOW_MS {
            continue;
        }
        if let Err(error) = refresh_account(entry.platform, &entry.account_id).await {
            logger::log_codex_api_warn(&format!(
                "[CodebuddyLocalAccess] 刷新账号令牌失败 account={} platform={} error={}",
                entry.account_id,
                entry.platform.as_str(),
                error
            ));
        }
    }
}

fn bind_host_for_scope(scope: CodebuddyLocalAccessScope) -> &'static str {
    match scope {
        CodebuddyLocalAccessScope::Localhost => LOCALHOST_BIND_HOST,
        CodebuddyLocalAccessScope::Lan => LAN_BIND_HOST,
    }
}

fn build_manifest(collection: &CodebuddyLocalAccessCollection) -> Result<Value, String> {
    let mut upstreams = Vec::new();
    for entry in &collection.accounts {
        let resolved = resolve_account(entry.platform, &entry.account_id)?;
        upstreams.push(json!({
            "id": entry.account_id,
            "uid": resolved.uid,
            "label": entry
                .label
                .clone()
                .filter(|value| !value.trim().is_empty())
                .unwrap_or_else(|| resolved.label.clone()),
            "platform": entry.platform.as_str(),
            "baseUrl": entry.platform.default_base_url(),
            "accessToken": resolved.access_token,
            "refreshToken": resolved.refresh_token,
            "includeReasoning": collection.include_reasoning,
            "realm": resolved.realm,
            "credits": resolved.credits_remain,
            "creditsExpiring": resolved.credits_size,
            "alwaysFreeModels": entry.always_free_models,
            "nightOnlyFreeModels": entry.night_only_free_models,
            "modelCatalog": collection
                .account_model_catalogs
                .iter()
                .find(|c| c.account_id == entry.account_id)
                .map(|c| c.models.clone())
                .unwrap_or_default(),
        }));
    }
    if upstreams.is_empty() {
        return Err("请至少选择一个 CodeBuddy / WorkBuddy 账号".to_string());
    }

    // Unified API keys — every key is equal; modelGroup binding sets accountIds/allowedModels.
    let mut api_keys = Vec::new();
    for entry in collection.all_api_keys() {
        let key = entry.key.trim();
        if key.is_empty() {
            continue;
        }
        let id = if entry.id.trim().is_empty() {
            format!("ck_{}", &key.chars().take(8).collect::<String>())
        } else {
            entry.id.clone()
        };
        let label = if entry.label.trim().is_empty() {
            id.clone()
        } else {
            entry.label.clone()
        };
        let mut account_ids: Vec<String> = Vec::new();
        let mut allowed_models: Vec<String> = Vec::new();
        if let Some(gid) = entry.model_group_id.as_ref().map(|s| s.trim()).filter(|s| !s.is_empty())
        {
            if let Some(group) = collection.model_group_by_id(gid) {
                for acc in &collection.accounts {
                    if group.contains_account(acc.platform, &acc.account_id) {
                        account_ids.push(acc.account_id.clone());
                    }
                }
                allowed_models = group.model_ids.clone();
            }
        }
        api_keys.push(json!({
            "id": id,
            "label": label,
            "key": key,
            "enabled": entry.enabled,
            "upstreamKind": "codebuddy",
            "modelGroupId": entry.model_group_id,
            "accountIds": account_ids,
            "allowedModels": allowed_models,
            "excludedModels": Vec::<String>::new(),
        }));
    }
    if api_keys.is_empty() {
        return Err("请至少配置一个 API Key".to_string());
    }

    let model_groups: Vec<Value> = collection
        .model_groups
        .iter()
        .map(|g| {
            json!({
                "id": g.id,
                "name": g.name,
                "accountIds": g.account_ids,
                "accountKeys": g.account_keys,
                "modelIds": g.model_ids,
                "kind": g.kind,
            })
        })
        .collect();

    let account_catalogs: Vec<Value> = collection
        .account_model_catalogs
        .iter()
        .map(|c| {
            json!({
                "accountId": c.account_id,
                "platform": c.platform,
                "label": c.label,
                "models": c.models,
            })
        })
        .collect();

    let mut model_efforts = serde_json::Map::new();
    let mut model_credits = serde_json::Map::new();
    for row in &collection.model_catalog {
        if let Some(obj) = row.as_object() {
            if let Some(id) = obj.get("id").and_then(|v| v.as_str()) {
                if let Some(efforts) = obj.get("efforts") {
                    model_efforts.insert(id.to_string(), efforts.clone());
                }
                if let Some(credits) = obj.get("credits") {
                    model_credits.insert(id.to_string(), credits.clone());
                }
            }
        }
    }
    // Merge per-account catalogs into global credits map (first wins).
    for cat in &collection.account_model_catalogs {
        for row in &cat.models {
            if let Some(obj) = row.as_object() {
                if let Some(id) = obj.get("id").and_then(|v| v.as_str()) {
                    if let Some(credits) = obj.get("credits") {
                        model_credits.entry(id.to_string()).or_insert_with(|| credits.clone());
                    }
                }
            }
        }
    }

    let mut manifest = json!({
        "locale": "zh-CN",
        "apiKeys": api_keys,
        "accounts": [],
        "modelIds": [],
        "modelGroups": model_groups,
        "accountModelCatalogs": account_catalogs,
        "routingStrategy": match collection.routing_strategy {
            crate::models::codebuddy_local_access::CodebuddyLocalAccessRoutingStrategy::Random => "random",
            _ => "round_robin",
        },
        "debugLogs": true,
        "codebuddyUpstreams": upstreams,
        "disabledModels": collection.disabled_models,
        "modelCatalog": collection.model_catalog,
        "modelEfforts": model_efforts,
        "modelCredits": model_credits,
        "costExploreInterval": "30m",
        "nightFreeEnabled": collection.night_free_enabled,
        "nightFreeModels": collection.night_free_models,
        "nightFreeStartHour": 23,
        "nightFreeEndHour": 8,
    });
    if !collection.model_ids.is_empty() {
        // Only override the account-level catalog when the user curated one.
        if let Some(list) = manifest
            .get_mut("codebuddyUpstreams")
            .and_then(|value| value.as_array_mut())
        {
            for upstream in list.iter_mut() {
                if let Some(object) = upstream.as_object_mut() {
                    object.insert("modelIds".to_string(), json!(collection.model_ids));
                }
            }
        }
    }
    Ok(manifest)
}

fn sidecar_config(collection: &CodebuddyLocalAccessCollection, auth_dir: &Path) -> Value {
    // CLIProxy 网关层只认 config 的 api-keys；客户端密钥也必须写进去，
    // 否则多把 key 会在鉴权中间件被 401，根本到不了 codebuddy manifest 策略。
    let mut api_keys = Vec::new();
    let primary = collection.api_key.trim();
    if !primary.is_empty() {
        api_keys.push(primary.to_string());
    }
    for entry in &collection.client_keys {
        let key = entry.key.trim();
        if key.is_empty() || !entry.enabled {
            continue;
        }
        if !api_keys.iter().any(|existing| existing == key) {
            api_keys.push(key.to_string());
        }
    }
    if api_keys.is_empty() {
        // 兜底：至少保留主 key 字段，避免空数组触发 safemode 模板检测异常。
        api_keys.push(collection.api_key.clone());
    }
    json!({
        "host": bind_host_for_scope(collection.access_scope),
        "port": collection.port,
        "auth-dir": auth_dir.to_string_lossy(),
        "debug": false,
        "api-keys": api_keys,
        "api-key-account-ids": [SIDECAR_API_KEY_ID],
        "commercial-mode": true,
        "ws-auth": true,
        "disable-auth-auto-refresh": true,
    })
}

fn write_runtime_files(
    collection: &CodebuddyLocalAccessCollection,
) -> Result<(PathBuf, PathBuf), String> {
    let dir = runtime_dir()?;
    std::fs::create_dir_all(&dir).map_err(|error| format!("创建运行目录失败: {}", error))?;
    let auth_dir = dir.join(SIDECAR_AUTHS_DIR);
    std::fs::create_dir_all(&auth_dir).map_err(|error| format!("创建认证目录失败: {}", error))?;

    let config_path = dir.join(SIDECAR_CONFIG_FILE);
    let manifest_path = dir.join(SIDECAR_MANIFEST_FILE);

    let config = sidecar_config(collection, &auth_dir);
    let config_text = serde_json::to_string_pretty(&config)
        .map_err(|error| format!("序列化配置失败: {}", error))?;
    atomic_write::write_string_atomic(&config_path, &config_text)?;

    let manifest = build_manifest(collection)?;
    let manifest_text = serde_json::to_string_pretty(&manifest)
        .map_err(|error| format!("序列化清单失败: {}", error))?;
    atomic_write::write_string_atomic(&manifest_path, &manifest_text)?;

    Ok((config_path, manifest_path))
}

/// Handle one JSON line emitted by the sidecar on stdout.
async fn handle_sidecar_event(collection: &CodebuddyLocalAccessCollection, line: &str) {
    let trimmed = line.trim();
    if !trimmed.starts_with('{') {
        return;
    }
    let Ok(payload) = serde_json::from_str::<Value>(trimmed) else {
        return;
    };
    match payload.get("type").and_then(Value::as_str) {
        Some("ready") => {
            if let Some(port) = payload.get("port").and_then(Value::as_u64) {
                let mut runtime = runtime().lock().await;
                runtime.port = Some(port as u16);
                runtime.bind_host = payload
                    .get("host")
                    .and_then(Value::as_str)
                    .map(str::to_string);
            }
        }
        Some("codebuddy_usage") => {
            match serde_json::from_value::<crate::models::codebuddy_local_access::CodebuddyRequestRecord>(
                payload.clone(),
            ) {
                Ok(record) => {
                    if let Err(error) =
                        crate::modules::codebuddy_local_access_request_logs::record_usage_event(
                            &record,
                        )
                    {
                        logger::log_codex_api_warn(&format!(
                            "[CodebuddyLocalAccess] 写入请求历史库失败: {}",
                            error
                        ));
                    }
                }
                Err(error) => logger::log_codex_api_warn(&format!(
                    "[CodebuddyLocalAccess] codebuddy_usage 事件解析失败: {}",
                    error
                )),
            }
        }
        Some("codebuddy_token_refreshed") => {
            let Some(upstream_id) = payload.get("upstreamId").and_then(Value::as_str) else {
                return;
            };
            let Some(access_token) = payload.get("accessToken").and_then(Value::as_str) else {
                return;
            };
            let refresh_token = payload.get("refreshToken").and_then(Value::as_str);
            let Some(entry) = collection
                .accounts
                .iter()
                .find(|entry| entry.account_id == upstream_id)
            else {
                return;
            };
            let expires_at_ms = jwt_exp_ms(access_token);
            match persist_rotated_tokens(
                entry.platform,
                upstream_id,
                access_token,
                refresh_token,
                expires_at_ms,
            ) {
                Ok(()) => logger::log_codex_api_info(&format!(
                    "[CodebuddyLocalAccess] 已回写续期令牌 account={}",
                    upstream_id
                )),
                Err(error) => logger::log_codex_api_warn(&format!(
                    "[CodebuddyLocalAccess] 回写续期令牌失败 account={} error={}",
                    upstream_id, error
                )),
            }
        }
        _ => {}
    }
}

fn spawn_sidecar_event_reader(
    child: &mut Child,
    collection: Arc<CodebuddyLocalAccessCollection>,
) -> Option<tokio::task::JoinHandle<()>> {
    let stdout = child.stdout.take()?;
    Some(tokio::spawn(async move {
        let mut lines = BufReader::new(stdout).lines();
        while let Ok(Some(line)) = lines.next_line().await {
            handle_sidecar_event(&collection, &line).await;
        }
    }))
}

async fn stop_sidecar_locked(runtime: &mut ServiceRuntime) {
    if let Some(mut child) = runtime.child.take() {
        let _ = child.kill().await;
        let _ = child.wait().await;
    }
    runtime.running = false;
    runtime.port = None;
}

async fn wait_for_ready(port: u16, api_key: &str) -> Result<(), String> {
    let url = format!("http://{}:{}/v1/models", CLIENT_URL_HOST, port);
    let client = reqwest::Client::builder()
        .timeout(Duration::from_millis(800))
        .build()
        .map_err(|error| format!("创建健康检查客户端失败: {}", error))?;
    let deadline = Instant::now() + SIDECAR_READY_TIMEOUT;
    let mut last_error = String::from("sidecar 未在超时时间内就绪");
    while Instant::now() < deadline {
        match client.get(&url).bearer_auth(api_key.trim()).send().await {
            Ok(response) if response.status().is_success() => return Ok(()),
            Ok(response) => last_error = format!("HTTP {}", response.status()),
            Err(error) => last_error = error.to_string(),
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    Err(last_error)
}

pub async fn start_service(collection: &CodebuddyLocalAccessCollection) -> Result<(), String> {
    let _guard = lifecycle_lock().lock().await;
    refresh_expiring_accounts(collection).await;
    let (config_path, manifest_path) = write_runtime_files(collection)?;
    let binary = sidecar_binary_path()?;

    {
        let mut runtime = runtime().lock().await;
        stop_sidecar_locked(&mut runtime).await;
    }

    let mut command = TokioCommand::new(&binary);
    sanitize_sidecar_command_env(&mut command);
    if let Ok(data_dir) = account::get_data_dir() {
        let state_dir = data_dir.join(RUNTIME_DIR);
        let _ = std::fs::create_dir_all(&state_dir);
        command.env("COCKPIT_CODEBUDDY_STATE_DIR", &state_dir);
        command.env("COCKPIT_TOOLS_DATA_DIR", &data_dir);
    }
    command
        .arg("--config")
        .arg(&config_path)
        .arg("--manifest")
        .arg(&manifest_path)
        .arg("--parent-pid")
        .arg(std::process::id().to_string())
        .current_dir(config_path.parent().unwrap_or_else(|| Path::new(".")))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    #[cfg(target_os = "windows")]
    {
        command.creation_flags(0x08000000);
    }

    let mut child = command
        .spawn()
        .map_err(|error| format!("启动 CodeBuddy API 服务失败: {}", error))?;
    let collection = Arc::new(collection.clone());
    if let Some(reader) = spawn_sidecar_event_reader(&mut child, Arc::clone(&collection)) {
        // The reader owns stdout; the child handle stays in the runtime.
        drop(reader);
    }

    {
        let mut runtime = runtime().lock().await;
        runtime.child = Some(child);
        runtime.running = true;
        runtime.port = Some(collection.port);
        runtime.bind_host = Some(bind_host_for_scope(collection.access_scope).to_string());
        runtime.last_error = None;
    }

    if let Err(error) = wait_for_ready(collection.port, &collection.api_key).await {
        let mut runtime = runtime().lock().await;
        stop_sidecar_locked(&mut runtime).await;
        runtime.last_error = Some(error.clone());
        logger::log_codex_api_error(&format!("[CodebuddyLocalAccess] 启动失败: {}", error));
        return Err(error);
    }
    logger::log_codex_api_info(&format!(
        "[CodebuddyLocalAccess] 服务已启动 port={} accounts={}",
        collection.port,
        collection.accounts.len()
    ));
    Ok(())
}

pub async fn stop_service() -> Result<(), String> {
    let _guard = lifecycle_lock().lock().await;
    let mut runtime = runtime().lock().await;
    stop_sidecar_locked(&mut runtime).await;
    runtime.last_error = None;
    Ok(())
}

pub async fn restart_service(collection: &CodebuddyLocalAccessCollection) -> Result<(), String> {
    stop_service().await?;
    start_service(collection).await
}

/// Bring the process in line with `collection.enabled`.
pub async fn reconcile() {
    let collection = load_collection();
    let running = {
        let runtime = runtime().lock().await;
        runtime.running
    };
    if collection.enabled && !running {
        if let Err(error) = start_service(&collection).await {
            logger::log_codex_api_warn(&format!("[CodebuddyLocalAccess] 自动启动失败: {}", error));
        }
    } else if !collection.enabled && running {
        let _ = stop_service().await;
    }
}

pub fn stop_service_on_shutdown() {
    if let Ok(mut runtime) = runtime().try_lock() {
        if let Some(child) = runtime.child.as_mut() {
            let _ = child.start_kill();
        }
        runtime.running = false;
    }
}

fn quota_credits_from_raw(quota_raw: Option<&serde_json::Value>) -> (Option<i64>, Option<i64>) {
    let Some(raw) = quota_raw else {
        return (None, None);
    };
    let mut remain_total = 0i64;
    let mut size_total = 0i64;
    let mut expiring_total = 0i64;
    let mut found = false;
    // soon window for "expiring" bucket (align workbuddy2api default 7d).
    let soon_ms = 7i64 * 24 * 3600 * 1000;
    let now_ms = chrono::Utc::now().timestamp_millis();
    let mut walk = |node: &serde_json::Value| {
        let obj = match node {
            serde_json::Value::Object(map) => map,
            _ => return,
        };
        let remain = obj
            .get("CycleCapacityRemainPrecise")
            .and_then(|v| v.as_str())
            .and_then(|s| s.trim().parse::<i64>().ok())
            .or_else(|| {
                obj.get("CycleCapacityRemain")
                    .and_then(serde_json::Value::as_i64)
            });
        let size = obj
            .get("CycleCapacitySize")
            .and_then(serde_json::Value::as_i64);
        if remain.is_some() || size.is_some() {
            found = true;
            let r = remain.unwrap_or(0).max(0);
            remain_total += r;
            size_total += size.unwrap_or(0);
            // Expiring bucket: CycleEndTime "2006-01-02 15:04:05" (UTC+8 wall clock)
            // within soon window. Missing/unparsable → stable (do not over-prioritize).
            if r > 0 {
                if let Some(end) = obj
                    .get("CycleEndTime")
                    .and_then(serde_json::Value::as_str)
                {
                    if let Ok(dt) = chrono::NaiveDateTime::parse_from_str(end, "%Y-%m-%d %H:%M:%S") {
                        use chrono::TimeZone;
                        let end_cst = chrono::FixedOffset::east_opt(8 * 3600)
                            .and_then(|tz| tz.from_local_datetime(&dt).single())
                            .map(|d| d.timestamp_millis())
                            .unwrap_or_else(|| dt.and_utc().timestamp_millis() + 8 * 3600 * 1000);
                        if end_cst - now_ms <= soon_ms {
                            expiring_total += r;
                        }
                    }
                }
            }
        }
    };
    fn collect_objects(node: &serde_json::Value, out: &mut Vec<serde_json::Map<String, serde_json::Value>>) {
        let obj = match node {
            serde_json::Value::Object(map) => map,
            serde_json::Value::Array(items) => {
                for item in items {
                    collect_objects(item, out);
                }
                return;
            }
            _ => return,
        };
        out.push(obj.clone());
        for value in obj.values() {
            collect_objects(value, out);
        }
    }
    let mut objects = Vec::new();
    collect_objects(raw, &mut objects);
    for obj in objects {
        walk(&serde_json::Value::Object(obj));
    }
    if !found {
        return (None, None);
    }
    // Return (remain, expiring) — size is not used as expiring by pool weight.
    let _ = size_total;
    (Some(remain_total.max(0)), Some(expiring_total.max(0).min(remain_total.max(0))))
}

fn account_options(
    collection: &CodebuddyLocalAccessCollection,
) -> Vec<CodebuddyLocalAccessAccountOption> {
    let selected: std::collections::HashSet<(String, String)> = collection
        .accounts
        .iter()
        .map(|entry| {
            (
                entry.platform.as_str().to_string(),
                entry.account_id.clone(),
            )
        })
        .collect();

    let mut options = Vec::new();
    let mut push = |platform: CodebuddyLocalAccessPlatform,
                    account_id: String,
                    label: String,
                    token: String,
                    expires_at: Option<i64>,
                    credits_remain: Option<i64>,
                    credits_size: Option<i64>| {
        let token = token.trim().to_string();
        let expires_at_ms = expires_at
            .filter(|value| *value > 0)
            .or_else(|| jwt_exp_ms(&token));
        options.push(CodebuddyLocalAccessAccountOption {
            platform,
            platform_name: platform.display_name().to_string(),
            account_id: account_id.clone(),
            label,
            selected: selected.contains(&(platform.as_str().to_string(), account_id)),
            token_available: !token.is_empty(),
            expires_at_ms,
            credits_remain,
            credits_size,
        });
    };

    for account in workbuddy_account::list_accounts() {
        let (remain, size) = quota_credits_from_raw(account.quota_raw.as_ref());
        push(
            CodebuddyLocalAccessPlatform::Workbuddy,
            account.id.clone(),
            account.email.clone(),
            account.access_token.clone(),
            account.expires_at,
            remain,
            size,
        );
    }
    for account in codebuddy_cn_account::list_accounts() {
        let (remain, size) = quota_credits_from_raw(account.quota_raw.as_ref());
        push(
            CodebuddyLocalAccessPlatform::CodebuddyCn,
            account.id.clone(),
            account.email.clone(),
            account.access_token.clone(),
            account.expires_at,
            remain,
            size,
        );
    }
    for account in codebuddy_account::list_accounts() {
        let (remain, size) = quota_credits_from_raw(account.quota_raw.as_ref());
        push(
            CodebuddyLocalAccessPlatform::Codebuddy,
            account.id.clone(),
            account.email.clone(),
            account.access_token.clone(),
            account.expires_at,
            remain,
            size,
        );
    }
    options
}

pub fn current_model_ids(collection: &CodebuddyLocalAccessCollection) -> Vec<String> {
    if !collection.model_ids.is_empty() {
        return collection.model_ids.clone();
    }
    CODEBUDDY_DEFAULT_MODEL_IDS
        .iter()
        .map(|value| value.to_string())
        .collect()
}

pub async fn service_state() -> CodebuddyLocalAccessState {
    let collection = load_collection();
    let (running, base_url, bind_host, last_error) = {
        let runtime = runtime().lock().await;
        (
            runtime.running,
            runtime.base_url(),
            runtime.bind_host.clone(),
            runtime.last_error.clone(),
        )
    };
    let lan_base_url = match bind_host.as_deref() {
        Some(LAN_BIND_HOST) => {
            let port = {
                let runtime = runtime().lock().await;
                runtime.port
            };
            port.map(|port| format!("http://<本机局域网IP>:{}", port))
        }
        _ => None,
    };
    CodebuddyLocalAccessState {
        available_accounts: account_options(&collection),
        model_ids: current_model_ids(&collection),
        collection,
        running,
        base_url,
        lan_base_url,
        last_error,
    }
}

/// Fetch governance + request ledger from the running sidecar (`/v1/codebuddy/status`).
pub async fn fetch_runtime_status() -> Result<
    crate::models::codebuddy_local_access::CodebuddyRuntimeStatus,
    String,
> {
    use crate::models::codebuddy_local_access::CodebuddyRuntimeStatus;
    let base_url = {
        let runtime = runtime().lock().await;
        if !runtime.running {
            None
        } else {
            runtime.base_url()
        }
    };
    let Some(base_url) = base_url else {
        return Err("服务未运行，请先启动服务".to_string());
    };
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("创建 HTTP 客户端失败: {}", e))?;
    let resp = client
        .get(format!("{}/v1/codebuddy/status", base_url))
        .send()
        .await
        .map_err(|e| format!("请求运行状态失败: {}", e))?;
    if !resp.status().is_success() {
        return Err(format!("运行状态接口返回 {}", resp.status()));
    }
    let value: serde_json::Value = resp
        .json()
        .await
        .map_err(|e| format!("解析运行状态失败: {}", e))?;
    let mut status: CodebuddyRuntimeStatus = serde_json::from_value(value.clone())
        .map_err(|e| format!("运行状态结构不兼容: {}", e))?;
    status.raw = Some(value);
    Ok(status)
}

/// Fetch a page of request-log records from `/v1/codebuddy/requests`.
pub async fn fetch_runtime_requests(
    offset: Option<u32>,
    limit: Option<u32>,
) -> Result<serde_json::Value, String> {
    let base_url = {
        let runtime = runtime().lock().await;
        if !runtime.running {
            None
        } else {
            runtime.base_url()
        }
    };
    let Some(base_url) = base_url else {
        return Err("服务未运行，请先启动服务".to_string());
    };
    let offset = offset.unwrap_or(0).min(500);
    // Backfill may request the full ring; UI pages stay much smaller.
    let limit = limit.unwrap_or(20).clamp(1, 500);
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("创建 HTTP 客户端失败: {}", e))?;
    let resp = client
        .get(format!(
            "{}/v1/codebuddy/requests?offset={}&limit={}",
            base_url, offset, limit
        ))
        .send()
        .await
        .map_err(|e| format!("请求流水接口失败: {}", e))?;
    if !resp.status().is_success() {
        return Err(format!("请求流水接口返回 {}", resp.status()));
    }
    resp.json()
        .await
        .map_err(|e| format!("解析请求流水失败: {}", e))
}

/// When the durable SQLite store is empty but the sidecar still holds a live
/// ring buffer, ingest those records once so history is not lost after upgrade.
pub async fn maybe_backfill_request_logs() {
    match crate::modules::codebuddy_local_access_request_logs::request_logs_count() {
        Ok(0) => {}
        Ok(_) => return,
        Err(error) => {
            logger::log_codex_api_warn(&format!(
                "[CodebuddyLocalAccess] 查询请求历史库失败，跳过回填: {}",
                error
            ));
            return;
        }
    }
    let Ok(value) = fetch_runtime_requests(None, Some(500)).await else {
        return;
    };
    let Some(records) = value.get("records").and_then(Value::as_array).cloned() else {
        return;
    };
    let parsed: Vec<crate::models::codebuddy_local_access::CodebuddyRequestRecord> = records
        .into_iter()
        .filter_map(|item| serde_json::from_value(item).ok())
        .collect();
    if parsed.is_empty() {
        return;
    }
    match crate::modules::codebuddy_local_access_request_logs::backfill_from_sidecar_records(
        &parsed,
    ) {
        Ok(count) if count > 0 => logger::log_codex_api_info(&format!(
            "[CodebuddyLocalAccess] 已从 sidecar 回填请求历史 {} 条",
            count
        )),
        Ok(_) => {}
        Err(error) => logger::log_codex_api_warn(&format!(
            "[CodebuddyLocalAccess] 回填请求历史失败: {}",
            error
        )),
    }
}

/// Probe the running service. Uses `/v1/models` so the check never spends
/// subscription credit.
pub async fn test_service() -> CodebuddyLocalAccessTestResult {
    let collection = load_collection();
    let (running, base_url) = {
        let runtime = runtime().lock().await;
        (runtime.running, runtime.base_url())
    };
    if !running {
        return CodebuddyLocalAccessTestResult {
            ok: false,
            status: None,
            message: "服务未运行".to_string(),
            latency_ms: None,
            content: None,
        };
    }
    let Some(base_url) = base_url else {
        return CodebuddyLocalAccessTestResult {
            ok: false,
            status: None,
            message: "服务端口未知".to_string(),
            latency_ms: None,
            content: None,
        };
    };

    let client = match reqwest::Client::builder()
        .timeout(Duration::from_secs(15))
        .build()
    {
        Ok(client) => client,
        Err(error) => {
            return CodebuddyLocalAccessTestResult {
                ok: false,
                status: None,
                message: format!("创建测试客户端失败: {}", error),
                latency_ms: None,
                content: None,
            }
        }
    };
    let started = Instant::now();
    match client
        .get(format!("{}/v1/models", base_url))
        .bearer_auth(collection.api_key.trim())
        .send()
        .await
    {
        Ok(response) => {
            let status = response.status();
            let body = response.text().await.unwrap_or_default();
            CodebuddyLocalAccessTestResult {
                ok: status.is_success(),
                status: Some(status.as_u16()),
                message: if status.is_success() {
                    "服务响应正常".to_string()
                } else {
                    format!("服务返回 HTTP {}", status.as_u16())
                },
                latency_ms: Some(started.elapsed().as_millis() as u64),
                content: Some(truncate(&body, 2000)),
            }
        }
        Err(error) => CodebuddyLocalAccessTestResult {
            ok: false,
            status: None,
            message: format!("请求失败: {}", error),
            latency_ms: Some(started.elapsed().as_millis() as u64),
            content: None,
        },
    }
}

fn truncate(value: &str, limit: usize) -> String {
    if value.chars().count() <= limit {
        return value.to_string();
    }
    value.chars().take(limit).collect()
}

/// Update the collection and synchronize the running process with it.
///
/// Enabling (or saving while enabled) always performs a controlled restart so
/// the regenerated manifest takes effect; `start_service` stops the previous
/// child first, so this is safe to call on every save.
pub async fn apply_collection(
    mut next: CodebuddyLocalAccessCollection,
) -> Result<CodebuddyLocalAccessState, String> {
    normalize_collection(&mut next);
    save_collection(&next)?;
    if next.enabled {
        start_service(&next).await?;
    } else {
        stop_service().await?;
    }
    Ok(service_state().await)
}

/// 仅更新客户端密钥（名称/开关/新增删除），**不重启** sidecar。
///
/// - 持久化 collection（UI 立刻看到新名称）
/// - 重写 manifest / config 运行文件（下次启动生效；config 的 api-keys
///   若 sidecar 配置 watcher 存活会热更新鉴权集合）
/// - 运行中进程保持不动，避免打断正在代理的请求
pub async fn apply_client_keys_no_restart(
    client_keys: Vec<crate::models::codebuddy_local_access::CodebuddyClientKey>,
) -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = load_collection();
    collection.client_keys = client_keys;
    normalize_collection(&mut collection);
    save_collection(&collection)?;
    let running = {
        let runtime = runtime().lock().await;
        runtime.running
    };
    if running {
        // 只落盘运行文件，不 stop/start。
        match write_runtime_files(&collection) {
            Ok(_) => {
                logger::log_codex_api_info(&format!(
                    "[CodebuddyLocalAccess] 客户端密钥已热更新（未重启） count={}",
                    collection.client_keys.len()
                ));
            }
            Err(error) => {
                logger::log_codex_api_warn(&format!(
                    "[CodebuddyLocalAccess] 热更新密钥运行文件失败（名称已保存）: {}",
                    error
                ));
            }
        }
    }
    Ok(service_state().await)
}

// ── 上游模型目录 + 探测 ────────────────────────────────────────
//
// 上游 CLI 暴露的可用模型不在 `/v3/config`（对非 IDE UA 返回 code 12403），
// 而在企业个人模型接口：
//   GET {chat_base}/console/enterprises/personal/models   国内版
//   GET {chat_base}/v2/enterprises/personal/models        国际版优先
// 该接口要求官方客户端指纹头（X-CodeBuddy-Request / X-Requested-With / Origin
// / Referer / WorkBuddy UA），否则会被风控拦掉。
//
// 返回信封 `{code, msg, data:{agents[], models[]}}`：
//   * `agents[name=="cli"].models` 才是网关（CLI）真正暴露的模型 id 列表；
//   * `models[]` 带显示名、上下文、输出上限、推理档位、多模态能力。
const CODEBUDDY_MODELS_PATH_CN: &str = "/console/enterprises/personal/models";
const CODEBUDDY_MODELS_PATH_GLOBAL: &str = "/v2/enterprises/personal/models";
const CODEBUDDY_CLIENT_VERSION: &str = "5.5.4";
const CODEBUDDY_CLI_VERSION: &str = "2.137.1";
const CODEBUDDY_MODELS_TIMEOUT: Duration = Duration::from_secs(25);

fn codebuddy_origin(platform: CodebuddyLocalAccessPlatform) -> &'static str {
    match platform {
        CodebuddyLocalAccessPlatform::Codebuddy => "https://www.workbuddy.ai",
        _ => "https://www.codebuddy.cn",
    }
}

/// Headers that make the request look like the official desktop client.
/// Verified live against `copilot.tencent.com` — without them the gateway
/// answers `code 12403 check ua`.
fn codebuddy_client_headers(
    platform: CodebuddyLocalAccessPlatform,
    access_token: &str,
) -> Vec<(&'static str, String)> {
    let origin = codebuddy_origin(platform);
    let ua = format!(
        "WorkBuddy/{v} WorkBuddy/{v} CLI/{cli}",
        v = CODEBUDDY_CLIENT_VERSION,
        cli = CODEBUDDY_CLI_VERSION
    );
    let language = match platform {
        CodebuddyLocalAccessPlatform::Codebuddy => "en-US",
        _ => "zh-CN",
    };
    vec![
        ("Content-Type", "application/json".to_string()),
        ("Accept", "application/json".to_string()),
        ("Accept-Language", language.to_string()),
        ("X-Requested-With", "XMLHttpRequest".to_string()),
        ("X-CodeBuddy-Request", "1".to_string()),
        ("User-Agent", ua),
        ("Origin", origin.to_string()),
        ("Referer", format!("{}/", origin)),
        ("X-Agent-Purpose", "conversation".to_string()),
        ("X-IDE-Name", "WorkBuddy".to_string()),
        ("X-IDE-Type", "WorkBuddy".to_string()),
        ("X-IDE-Version", CODEBUDDY_CLIENT_VERSION.to_string()),
        ("X-Product", "WorkBuddy".to_string()),
        ("Authorization", format!("Bearer {}", access_token)),
    ]
}

fn codebuddy_model_paths(platform: CodebuddyLocalAccessPlatform) -> [&'static str; 2] {
    match platform {
        CodebuddyLocalAccessPlatform::Codebuddy => {
            [CODEBUDDY_MODELS_PATH_GLOBAL, CODEBUDDY_MODELS_PATH_CN]
        }
        _ => [CODEBUDDY_MODELS_PATH_CN, CODEBUDDY_MODELS_PATH_GLOBAL],
    }
}

/// Non-chat models must be filtered out: selecting one makes the upstream
/// answer `code 11102`. Mirrors the upstream `nonChatModel` rules.
fn is_non_chat_model(id: &str, max_output_tokens: i64, tags: &[String]) -> bool {
    let lowered = id.trim().to_ascii_lowercase();
    if ["nes-", "completion-", "codewise-"]
        .iter()
        .any(|prefix| lowered.starts_with(prefix))
    {
        return true;
    }
    if max_output_tokens > 0 && max_output_tokens <= 256 {
        return true;
    }
    tags.iter().any(|tag| tag == "text-to-image")
}

fn parse_credits_rate(raw: Option<&str>) -> Option<f64> {
    let s = raw?.trim().to_ascii_lowercase();
    let s = s.strip_prefix('x').unwrap_or(&s);
    let s = s.trim_end_matches("credits").trim();
    s.parse::<f64>().ok()
}

fn parse_codebuddy_models(payload: &Value) -> Result<(Vec<String>, Vec<CodebuddyModelInfo>), String> {
    let code = payload.get("code").and_then(Value::as_i64).unwrap_or(-1);
    if code != 0 {
        let message = payload
            .get("msg")
            .and_then(Value::as_str)
            .unwrap_or("模型接口返回非 0")
            .to_string();
        return Err(format!("code={} {}", code, message));
    }
    let data = payload
        .get("data")
        .ok_or_else(|| "模型接口缺少 data".to_string())?;

    // `cli` agent 暴露的模型才是网关可用的。
    let cli_ids: Vec<String> = data
        .get("agents")
        .and_then(Value::as_array)
        .and_then(|agents| {
            agents.iter().find_map(|agent| {
                if agent.get("name").and_then(Value::as_str) == Some("cli") {
                    agent.get("models").and_then(Value::as_array).map(|ids| {
                        ids.iter()
                            .filter_map(|id| id.as_str())
                            .map(|id| id.trim().to_string())
                            .filter(|id| !id.is_empty())
                            .collect()
                    })
                } else {
                    None
                }
            })
        })
        .unwrap_or_default();

    let mut infos: Vec<CodebuddyModelInfo> = Vec::new();
    let mut seen = std::collections::HashSet::new();
    for raw in data
        .get("models")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let id = raw
            .get("id")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .trim()
            .to_string();
        if id.is_empty() || !seen.insert(id.clone()) {
            continue;
        }
        if raw.get("disabled").and_then(Value::as_bool).unwrap_or(false) {
            continue;
        }
        let max_output = raw
            .get("maxOutputTokens")
            .and_then(Value::as_i64)
            .unwrap_or(0);
        let tags: Vec<String> = raw
            .get("tags")
            .and_then(Value::as_array)
            .map(|tags| {
                tags.iter()
                    .filter_map(|tag| tag.as_str())
                    .map(|tag| tag.to_string())
                    .collect()
            })
            .unwrap_or_default();
        if is_non_chat_model(&id, max_output, &tags) {
            continue;
        }
        let reasoning = raw.get("reasoning");
        let efforts: Vec<String> = reasoning
            .and_then(|value| value.get("supportedEfforts"))
            .and_then(Value::as_array)
            .map(|efforts| {
                efforts
                    .iter()
                    .filter_map(|value| value.as_str())
                    .map(|value| value.to_string())
                    .collect()
            })
            .unwrap_or_default();
        let description = raw
            .get("descriptionZh")
            .and_then(Value::as_str)
            .or_else(|| raw.get("descriptionEn").and_then(Value::as_str))
            .map(|value| value.trim().to_string())
            .filter(|value| !value.is_empty());
        infos.push(CodebuddyModelInfo {
            cli: cli_ids.iter().any(|candidate| candidate == &id),
            id,
            name: raw
                .get("name")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .trim()
                .to_string(),
            context_length: raw
                .get("maxInputTokens")
                .and_then(Value::as_i64)
                .unwrap_or(0),
            max_output_tokens: max_output,
            supports_images: raw
                .get("supportsImages")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            supports_reasoning: raw
                .get("supportsReasoning")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            supports_tool_call: raw
                .get("supportsToolCall")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            efforts,
            description,
            credits: raw
                .get("credits")
                .and_then(Value::as_str)
                .map(|s| s.trim().to_string())
                .filter(|s| !s.is_empty()),
            credits_rate: parse_credits_rate(raw.get("credits").and_then(Value::as_str)),
            tags: tags.clone(),
            vendor: raw
                .get("vendor")
                .and_then(Value::as_str)
                .map(|s| s.trim().to_string())
                .filter(|s| !s.is_empty()),
            is_default: raw.get("isDefault").and_then(Value::as_bool).unwrap_or(false),
            only_reasoning: raw
                .get("onlyReasoning")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            max_allowed_size: raw.get("maxAllowedSize").and_then(Value::as_i64).unwrap_or(0),
        });
    }

    // 展示顺序：cli 顺序优先，其余按上游顺序跟在后面。
    let mut ordered: Vec<CodebuddyModelInfo> = Vec::new();
    for id in &cli_ids {
        if let Some(position) = infos.iter().position(|info| &info.id == id) {
            ordered.push(infos.remove(position));
        }
    }
    ordered.append(&mut infos);
    Ok((cli_ids, ordered))
}

async fn fetch_models_for_account(
    client: &reqwest::Client,
    entry: &CodebuddyLocalAccessAccountRef,
) -> Result<Vec<CodebuddyModelInfo>, String> {
    let resolved = resolve_account(entry.platform, &entry.account_id)?;
    let base = entry.platform.default_base_url().trim_end_matches('/');
    let headers = codebuddy_client_headers(entry.platform, &resolved.access_token);
    let mut last_error = String::from("模型接口无响应");
    for path in codebuddy_model_paths(entry.platform) {
        let mut request = client.get(format!("{}{}", base, path));
        for (name, value) in &headers {
            request = request.header(*name, value);
        }
        let response = match request.send().await {
            Ok(response) => response,
            Err(error) => {
                last_error = format!("请求失败: {}", error);
                continue;
            }
        };
        let status = response.status();
        let body = response.text().await.unwrap_or_default();
        let payload: Value = match serde_json::from_str(&body) {
            Ok(payload) => payload,
            Err(_) => {
                last_error = format!("HTTP {} 响应无法解析", status.as_u16());
                continue;
            }
        };
        match parse_codebuddy_models(&payload) {
            Ok((_, models)) if !models.is_empty() => return Ok(models),
            Ok(_) => last_error = "模型接口未返回任何可用模型".to_string(),
            Err(error) => last_error = error,
        }
    }
    Err(last_error)
}

/// 拉取所选账号可用的真实模型列表（并集，cli 顺序优先）。
pub async fn fetch_models() -> CodebuddyFetchModelsResult {
    let collection = load_collection();
    if collection.accounts.is_empty() {
        return CodebuddyFetchModelsResult {
            ok: false,
            message: "请先勾选至少一个账号".to_string(),
            models: Vec::new(),
            accounts: Vec::new(),
        };
    }
    let client = match reqwest::Client::builder()
        .timeout(CODEBUDDY_MODELS_TIMEOUT)
        .connect_timeout(Duration::from_secs(6))
        .build()
    {
        Ok(client) => client,
        Err(error) => {
            return CodebuddyFetchModelsResult {
                ok: false,
                message: format!("创建请求客户端失败: {}", error),
                models: Vec::new(),
                accounts: Vec::new(),
            }
        }
    };

    let mut merged: Vec<CodebuddyModelInfo> = Vec::new();
    let mut accounts = Vec::new();
    for entry in &collection.accounts {
        let label = entry
            .label
            .clone()
            .filter(|value| !value.trim().is_empty())
            .unwrap_or_else(|| entry.account_id.clone());
        match fetch_models_for_account(&client, entry).await {
            Ok(models) => {
                let count = models.len();
                for model in models {
                    match merged.iter_mut().find(|existing| existing.id == model.id) {
                        Some(existing) => {
                            existing.cli = existing.cli || model.cli;
                            if existing.credits.is_none() {
                                existing.credits = model.credits;
                                existing.credits_rate = model.credits_rate;
                            }
                            if existing.tags.is_empty() {
                                existing.tags = model.tags;
                            }
                            if existing.description.is_none() {
                                existing.description = model.description;
                            }
                            if existing.vendor.is_none() {
                                existing.vendor = model.vendor;
                            }
                        }
                        None => merged.push(model),
                    }
                }
                accounts.push(CodebuddyFetchModelsAccountResult {
                    account_id: entry.account_id.clone(),
                    platform: entry.platform,
                    label,
                    ok: true,
                    message: format!("拉取到 {} 个模型", count),
                    count,
                });
            }
            Err(error) => accounts.push(CodebuddyFetchModelsAccountResult {
                account_id: entry.account_id.clone(),
                platform: entry.platform,
                label,
                ok: false,
                message: error,
                count: 0,
            }),
        }
    }

    // cli 模型排前面，保持上游顺序。
    let (mut cli, mut rest): (Vec<_>, Vec<_>) = merged.drain(..).partition(|model| model.cli);
    cli.append(&mut rest);
    let ok = !cli.is_empty() || !rest.is_empty();
    let failed = accounts.iter().filter(|account| !account.ok).count();
    let message = if !ok {
        format!("拉取失败（{} 个账号）", failed)
    } else if failed > 0 {
        format!("部分账号拉取失败（{}/{}）", failed, accounts.len())
    } else {
        format!("拉取到 {} 个可用模型", cli.len() + rest.len())
    };
    let catalog: Vec<serde_json::Value> = merged
        .iter()
        .map(|m| {
            serde_json::json!({
                "id": m.id,
                "name": m.name,
                "contextLength": m.context_length,
                "maxOutputTokens": m.max_output_tokens,
                "supportsImages": m.supports_images,
                "supportsReasoning": m.supports_reasoning,
                "supportsToolCall": m.supports_tool_call,
                "efforts": m.efforts,
                "description": m.description,
                "cli": m.cli,
                "credits": m.credits,
                "creditsRate": m.credits_rate,
                "tags": m.tags,
                "vendor": m.vendor,
                "isDefault": m.is_default,
                "onlyReasoning": m.only_reasoning,
                "maxAllowedSize": m.max_allowed_size,
            })
        })
        .collect();
    {
        let mut collection = load_collection();
        collection.model_catalog = catalog.clone();
        // Per-account catalogs for diff / free-binding.
        let mut per_account: Vec<crate::models::codebuddy_local_access::CodebuddyAccountModelCatalog> =
            Vec::new();
        for entry in &collection.accounts {
            let label = entry
                .label
                .clone()
                .filter(|v| !v.trim().is_empty())
                .unwrap_or_else(|| entry.account_id.clone());
            if let Ok(models) = fetch_models_for_account(&client, entry).await {
                let rows: Vec<serde_json::Value> = models
                    .iter()
                    .map(|m| {
                        serde_json::json!({
                            "id": m.id,
                            "name": m.name,
                            "credits": m.credits,
                            "creditsRate": m.credits_rate,
                            "tags": m.tags,
                            "contextLength": m.context_length,
                            "maxOutputTokens": m.max_output_tokens,
                            "efforts": m.efforts,
                            "cli": m.cli,
                            "description": m.description,
                        })
                    })
                    .collect();
                per_account.push(crate::models::codebuddy_local_access::CodebuddyAccountModelCatalog {
                    account_id: entry.account_id.clone(),
                    platform: Some(entry.platform.as_str().to_string()),
                    label: Some(label),
                    models: rows,
                });
            }
        }
        if !per_account.is_empty() {
            collection.account_model_catalogs = per_account;
        }
        let _ = save_collection(&collection);
    }
    CodebuddyFetchModelsResult {
        ok,
        message,
        models: cli.into_iter().chain(rest).collect(),
        accounts,
    }
}

/// Model catalog payload for UI: collection cache + disabled flags + pool/status extras.
pub async fn model_catalog_payload() -> serde_json::Value {
    let collection = load_collection();
    let mut models = collection.model_catalog.clone();
    if models.is_empty() {
        for id in current_model_ids(&collection) {
            models.push(serde_json::json!({ "id": id, "name": id, "cli": true }));
        }
    }
    let disabled: Vec<String> = collection.disabled_models.clone();
    // Enrich from sidecar /status when running (free/paid + usage + cost explore).
    let mut runtime = serde_json::Value::Null;
    if let Ok(status) = fetch_runtime_status().await {
        if let Some(raw) = status.raw.clone() {
            runtime = raw;
        }
    }
    serde_json::json!({
        "models": models,
        "disabledModels": disabled,
        "runtime": runtime,
        "collectionModelIds": collection.model_ids,
        // Full enterprise rows when available (credits/tags/vendor…).
        "modelCatalog": collection.model_catalog,
    })
}

/// Query sidecar request ledger filtered by API key.
pub async fn query_runtime_requests_filtered(
    api_key: Option<String>,
    offset: Option<u32>,
    limit: Option<u32>,
) -> Result<serde_json::Value, String> {
    let base_url = {
        let runtime = runtime().lock().await;
        if !runtime.running {
            None
        } else {
            runtime.base_url()
        }
    };
    let Some(base_url) = base_url else {
        return Err("服务未运行，请先启动服务".to_string());
    };
    let offset = offset.unwrap_or(0).min(500);
    let limit = limit.unwrap_or(20).clamp(1, 500);
    let mut url = format!("{}/v1/codebuddy/requests?offset={}&limit={}", base_url, offset, limit);
    if let Some(key) = api_key {
        let key = key.trim();
        if !key.is_empty() {
            url.push_str(&format!("&apiKey={}", urlencoding_lite(key)));
        }
    }
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("创建 HTTP 客户端失败: {}", e))?;
    let resp = client
        .get(&url)
        .send()
        .await
        .map_err(|e| format!("请求流水失败: {}", e))?;
    if !resp.status().is_success() {
        return Err(format!("请求流水接口返回 {}", resp.status()));
    }
    resp.json()
        .await
        .map_err(|e| format!("解析请求流水失败: {}", e))
}

fn urlencoding_lite(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{:02X}", b)),
        }
    }
    out
}

/// API key usage stats from sidecar (running) or empty.
pub async fn api_key_stats_payload() -> serde_json::Value {
    let base_url = {
        let runtime = runtime().lock().await;
        if runtime.running {
            runtime.base_url()
        } else {
            None
        }
    };
    let Some(base_url) = base_url else {
        return serde_json::json!({ "apiKeyStats": {}, "clientKeys": load_collection().client_keys });
    };
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .ok();
    let mut stats = serde_json::json!({});
    if let Some(client) = client {
        if let Ok(resp) = client
            .get(format!("{}/v1/codebuddy/status", base_url))
            .send()
            .await
        {
            if let Ok(value) = resp.json::<serde_json::Value>().await {
                if let Some(obj) = value.get("apiKeyStats") {
                    stats = obj.clone();
                }
            }
        }
    }
    serde_json::json!({
        "apiKeyStats": stats,
        "clientKeys": load_collection().client_keys,
        "primaryKeyLabel": "Primary",
    })
}

/// Clear sticky session bindings on the running sidecar.
pub async fn clear_sticky_sessions() -> Result<serde_json::Value, String> {
    let base_url = {
        let runtime = runtime().lock().await;
        if runtime.running {
            runtime.base_url()
        } else {
            None
        }
    };
    let Some(base_url) = base_url else {
        return Err("服务未运行".to_string());
    };
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("创建 HTTP 客户端失败: {}", e))?;
    let resp = client
        .post(format!("{}/v1/codebuddy/sessions/clear", base_url))
        .send()
        .await
        .map_err(|e| format!("清除会话粘性失败: {}", e))?;
    if !resp.status().is_success() {
        return Err(format!("清除会话粘性返回 {}", resp.status()));
    }
    resp.json()
        .await
        .map_err(|e| format!("解析结果失败: {}", e))
}

/// 经本地服务发起一次真实对话，验证「模型 + 账号」端到端是否可用。
pub async fn probe_chat(model: String, prompt: Option<String>) -> CodebuddyProbeResult {
    let model = model.trim().to_string();
    if model.is_empty() {
        return CodebuddyProbeResult {
            ok: false,
            model,
            status: None,
            message: "请先选择要探测的模型".to_string(),
            latency_ms: None,
            content: None,
            reasoning: None,
            total_tokens: None,
        };
    }
    let collection = load_collection();
    let base_url = {
        let runtime = runtime().lock().await;
        if !runtime.running {
            None
        } else {
            runtime.base_url()
        }
    };
    let Some(base_url) = base_url else {
        return CodebuddyProbeResult {
            ok: false,
            model,
            status: None,
            message: "服务未运行，请先启动服务".to_string(),
            latency_ms: None,
            content: None,
            reasoning: None,
            total_tokens: None,
        };
    };

    let content = prompt
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| "请只回复两个字：你好".to_string());
    let body = json!({
        "model": model,
        "messages": [{ "role": "user", "content": content }],
        "stream": false,
        "max_tokens": 128,
    });

    let client = match reqwest::Client::builder()
        .timeout(Duration::from_secs(60))
        .build()
    {
        Ok(client) => client,
        Err(error) => {
            return CodebuddyProbeResult {
                ok: false,
                model,
                status: None,
                message: format!("创建测试客户端失败: {}", error),
                latency_ms: None,
                content: None,
                reasoning: None,
                total_tokens: None,
            }
        }
    };

    let started = Instant::now();
    let response = client
        .post(format!("{}/v1/chat/completions", base_url))
        .bearer_auth(collection.api_key.trim())
        .json(&body)
        .send()
        .await;
    let latency = started.elapsed().as_millis() as u64;
    let response = match response {
        Ok(response) => response,
        Err(error) => {
            return CodebuddyProbeResult {
                ok: false,
                model,
                status: None,
                message: format!("请求失败: {}", error),
                latency_ms: Some(latency),
                content: None,
                reasoning: None,
                total_tokens: None,
            }
        }
    };
    let status = response.status();
    let raw = response.text().await.unwrap_or_default();
    let payload: Value = serde_json::from_str(&raw).unwrap_or(Value::Null);

    if !status.is_success() {
        let message = payload
            .get("error")
            .and_then(|error| error.get("message"))
            .and_then(Value::as_str)
            .map(|value| value.to_string())
            .unwrap_or_else(|| format!("服务返回 HTTP {}", status.as_u16()));
        return CodebuddyProbeResult {
            ok: false,
            model,
            status: Some(status.as_u16()),
            message,
            latency_ms: Some(latency),
            content: None,
            reasoning: None,
            total_tokens: None,
        };
    }

    let choice = payload
        .get("choices")
        .and_then(Value::as_array)
        .and_then(|choices| choices.first());
    let message = choice.and_then(|choice| choice.get("message"));
    let reply = message
        .and_then(|message| message.get("content"))
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim()
        .to_string();
    let reasoning = message
        .and_then(|message| message.get("reasoning_content"))
        .and_then(Value::as_str)
        .map(|value| truncate(value.trim(), 2000))
        .filter(|value| !value.is_empty());
    let total_tokens = payload
        .get("usage")
        .and_then(|usage| usage.get("total_tokens"))
        .and_then(Value::as_i64);

    CodebuddyProbeResult {
        ok: true,
        model,
        status: Some(status.as_u16()),
        message: format!("模型响应正常 · {}ms", latency),
        latency_ms: Some(latency),
        content: Some(truncate(&reply, 2000)),
        reasoning,
        total_tokens,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::codebuddy_local_access::CodebuddyLocalAccessAccountRef;

    fn build_account_ref(
        platform: CodebuddyLocalAccessPlatform,
        account_id: &str,
        label: Option<String>,
    ) -> CodebuddyLocalAccessAccountRef {
        CodebuddyLocalAccessAccountRef {
            platform,
            account_id: account_id.trim().to_string(),
            label: label.filter(|value| !value.trim().is_empty()),
        }
    }

    #[test]
    fn default_collection_is_disabled_and_portable() {
        let collection = CodebuddyLocalAccessCollection::default();
        assert!(!collection.enabled);
        assert!(collection.port > 0);
        assert!(collection.accounts.is_empty());
    }

    #[test]
    fn normalize_collection_generates_api_key_and_dedupes_accounts() {
        let mut collection = CodebuddyLocalAccessCollection::default();
        collection.accounts = vec![
            build_account_ref(CodebuddyLocalAccessPlatform::Workbuddy, "a1", None),
            build_account_ref(CodebuddyLocalAccessPlatform::Workbuddy, " a1 ", None),
            build_account_ref(CodebuddyLocalAccessPlatform::CodebuddyCn, "a1", None),
        ];
        collection.model_ids = vec![" glm-5.1 ".to_string(), "GLM-5.1".to_string()];
        normalize_collection(&mut collection);

        assert!(collection.api_key.starts_with("cbk-"));
        assert_eq!(collection.accounts.len(), 2);
        assert_eq!(collection.model_ids, vec!["glm-5.1".to_string()]);
    }

    #[test]
    fn jwt_exp_is_decoded_in_milliseconds() {
        // {"exp":1789030679} base64url payload
        let payload = URL_SAFE_NO_PAD.encode(br#"{"exp":1789030679}"#);
        let token = format!("header.{}.signature", payload);
        assert_eq!(jwt_exp_ms(&token), Some(1789030679000));
        assert_eq!(jwt_exp_ms("not-a-jwt"), None);
    }

    #[test]
    fn build_manifest_requires_at_least_one_account() {
        let mut collection = CodebuddyLocalAccessCollection::default();
        normalize_collection(&mut collection);
        let error = build_manifest(&collection).expect_err("empty selection must fail");
        assert!(error.contains("至少选择一个"));
    }

    #[test]
    fn sidecar_config_binds_loopback_for_localhost_scope() {
        let mut collection = CodebuddyLocalAccessCollection::default();
        normalize_collection(&mut collection);
        let config = sidecar_config(&collection, Path::new("/tmp/auths"));
        assert_eq!(config["host"], LOCALHOST_BIND_HOST);
        assert_eq!(config["port"], json!(collection.port));

        collection.access_scope = CodebuddyLocalAccessScope::Lan;
        let config = sidecar_config(&collection, Path::new("/tmp/auths"));
        assert_eq!(config["host"], LAN_BIND_HOST);
    }

    #[test]
    fn current_model_ids_falls_back_to_preset() {
        let mut collection = CodebuddyLocalAccessCollection::default();
        assert_eq!(
            current_model_ids(&collection).len(),
            CODEBUDDY_DEFAULT_MODEL_IDS.len()
        );
        collection.model_ids = vec!["custom-model".to_string()];
        assert_eq!(
            current_model_ids(&collection),
            vec!["custom-model".to_string()]
        );
    }
}
