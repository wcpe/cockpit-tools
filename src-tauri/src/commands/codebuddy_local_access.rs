// CodeBuddy / WorkBuddy 本地 API 服务（反代）命令。
// 与 Codex API Service 命令保持同一契约风格：Result<T, String> + camelCase 参数。

use crate::models::codebuddy_local_access::{
    CodebuddyClientKey, CodebuddyFetchModelsResult, CodebuddyLocalAccessAccountRef,
    CodebuddyLocalAccessPlatform, CodebuddyLocalAccessState, CodebuddyLocalAccessTestResult,
    CodebuddyProbeResult, CodebuddyRequestLogPage, CodebuddyUsageStats,
};
use crate::modules::codebuddy_local_access;
use crate::modules::codebuddy_local_access_request_logs;

#[tauri::command]
pub async fn codebuddy_local_access_get_state() -> Result<CodebuddyLocalAccessState, String> {
    Ok(codebuddy_local_access::service_state().await)
}

/// Persist the collection and, when enabled, (re)start the service so the
/// regenerated manifest takes effect.
#[tauri::command]
pub async fn codebuddy_local_access_save(
    enabled: Option<bool>,
    port: Option<u16>,
    access_scope: Option<String>,
    include_reasoning: Option<bool>,
    routing_strategy: Option<String>,
    accounts: Option<Vec<CodebuddyLocalAccessAccountRef>>,
    model_ids: Option<Vec<String>>,
    client_keys: Option<Vec<CodebuddyClientKey>>,
) -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = codebuddy_local_access::load_collection();
    if let Some(enabled) = enabled {
        collection.enabled = enabled;
    }
    if let Some(port) = port {
        collection.port = port;
    }
    if let Some(scope) = access_scope.as_deref() {
        collection.access_scope = match scope {
            "lan" => crate::models::codebuddy_local_access::CodebuddyLocalAccessScope::Lan,
            _ => crate::models::codebuddy_local_access::CodebuddyLocalAccessScope::Localhost,
        };
    }
    if let Some(include_reasoning) = include_reasoning {
        collection.include_reasoning = include_reasoning;
    }
    if let Some(strategy) = routing_strategy.as_deref() {
        collection.routing_strategy = match strategy {
            "random" => {
                crate::models::codebuddy_local_access::CodebuddyLocalAccessRoutingStrategy::Random
            }
            _ => crate::models::codebuddy_local_access::CodebuddyLocalAccessRoutingStrategy::RoundRobin,
        };
    }
    if let Some(accounts) = accounts {
        collection.accounts = accounts;
    }
    if let Some(model_ids) = model_ids {
        collection.model_ids = model_ids;
    }
    if let Some(client_keys) = client_keys {
        collection.client_keys = client_keys;
    }
    codebuddy_local_access::apply_collection(collection).await
}

/// Start the service with the persisted collection (regenerating the manifest).
#[tauri::command]
pub async fn codebuddy_local_access_start() -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = codebuddy_local_access::load_collection();
    collection.enabled = true;
    codebuddy_local_access::apply_collection(collection).await
}

/// 更新客户端密钥（改名/增删/开关），不重启 sidecar。
#[tauri::command]
pub async fn codebuddy_local_access_save_client_keys(
    client_keys: Vec<CodebuddyClientKey>,
) -> Result<CodebuddyLocalAccessState, String> {
    codebuddy_local_access::apply_client_keys_no_restart(client_keys).await
}

#[tauri::command]
pub async fn codebuddy_local_access_stop() -> Result<CodebuddyLocalAccessState, String> {
    codebuddy_local_access::stop_service().await?;
    Ok(codebuddy_local_access::service_state().await)
}

#[tauri::command]
pub async fn codebuddy_local_access_restart() -> Result<CodebuddyLocalAccessState, String> {
    let collection = codebuddy_local_access::load_collection();
    codebuddy_local_access::restart_service(&collection).await?;
    Ok(codebuddy_local_access::service_state().await)
}

/// Regenerate the API key without touching service state.
#[tauri::command]
pub async fn codebuddy_local_access_rotate_api_key() -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = codebuddy_local_access::load_collection();
    collection.api_key = String::new();
    codebuddy_local_access::apply_collection(collection).await
}

/// Probe the local `/v1/models` endpoint. Never spends subscription credit.
#[tauri::command]
pub async fn codebuddy_local_access_test() -> Result<CodebuddyLocalAccessTestResult, String> {
    Ok(codebuddy_local_access::test_service().await)
}

/// Governance ledger + request log from the running sidecar.
#[tauri::command]
pub async fn codebuddy_local_access_runtime_status(
) -> Result<crate::models::codebuddy_local_access::CodebuddyRuntimeStatus, String> {
    codebuddy_local_access::fetch_runtime_status().await
}

/// Paginated request-log window from the running sidecar.
#[tauri::command]
pub async fn codebuddy_local_access_runtime_requests(
    offset: Option<u32>,
    limit: Option<u32>,
) -> Result<serde_json::Value, String> {
    codebuddy_local_access::fetch_runtime_requests(offset, limit).await
}

/// Paginated durable request history from SQLite (survives sidecar restarts).
#[tauri::command]
pub async fn codebuddy_local_access_query_request_logs(
    offset: Option<u32>,
    limit: Option<u32>,
) -> Result<CodebuddyRequestLogPage, String> {
    codebuddy_local_access::maybe_backfill_request_logs().await;
    codebuddy_local_access_request_logs::query_request_logs_page(
        offset.unwrap_or(0),
        limit.unwrap_or(20),
    )
}

/// Aggregate usage stats (by model / account) from the durable SQLite store.
#[tauri::command]
pub async fn codebuddy_local_access_query_usage_stats() -> Result<CodebuddyUsageStats, String> {
    codebuddy_local_access::maybe_backfill_request_logs().await;
    codebuddy_local_access_request_logs::query_usage_stats()
}

/// Clear the durable CodeBuddy request history database.
#[tauri::command]
pub async fn codebuddy_local_access_clear_request_logs() -> Result<(), String> {
    codebuddy_local_access_request_logs::clear_request_logs()
}

/// 直连上游企业模型接口，拉取所选账号真实可用的模型（含显示名/上下文/推理档位）。
#[tauri::command]
pub async fn codebuddy_local_access_fetch_models() -> Result<CodebuddyFetchModelsResult, String> {
    Ok(codebuddy_local_access::fetch_models().await)
}

/// 经本地服务发起一次真实对话，验证「模型」是否真的能响应。
#[tauri::command]
pub async fn codebuddy_local_access_probe_chat(
    model: String,
    prompt: Option<String>,
) -> Result<CodebuddyProbeResult, String> {
    Ok(codebuddy_local_access::probe_chat(model, prompt).await)
}

/// Expose the supported default model catalog for the UI.
#[tauri::command]
pub fn codebuddy_local_access_default_models() -> Vec<String> {
    crate::models::codebuddy_local_access::CODEBUDDY_DEFAULT_MODEL_IDS
        .iter()
        .map(|value| value.to_string())
        .collect()
}

#[tauri::command]
pub async fn codebuddy_local_access_save_api_keys(
    client_keys: Vec<crate::models::codebuddy_local_access::CodebuddyClientKey>,
) -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = codebuddy_local_access::load_collection();
    // Unified keys: if legacy api_key is not in the list, keep it as first key.
    collection.client_keys = client_keys
        .into_iter()
        .filter(|k| !k.key.trim().is_empty())
        .collect();
    if collection.api_key.trim().is_empty() {
        if let Some(first) = collection.client_keys.first() {
            collection.api_key = first.key.clone();
        }
    }
    codebuddy_local_access::apply_collection(collection).await
}

#[tauri::command]
pub async fn codebuddy_local_access_set_model_groups(
    model_groups: Vec<crate::models::codebuddy_local_access::CodebuddyModelGroup>,
) -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = codebuddy_local_access::load_collection();
    collection.model_groups = model_groups
        .into_iter()
        .map(|mut g| {
            g.id = g.id.trim().to_string();
            if g.id.is_empty() {
                g.id = format!("grp_{}", std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map(|d| d.as_millis())
                    .unwrap_or(0));
            }
            if g.name.trim().is_empty() {
                g.name = g.id.clone();
            }
            g.account_ids.retain(|s| !s.trim().is_empty());
            g.model_ids.retain(|s| !s.trim().is_empty());
            g
        })
        .collect();
    codebuddy_local_access::apply_collection(collection).await
}

#[tauri::command]
pub async fn codebuddy_local_access_get_model_groups() -> Result<serde_json::Value, String> {
    let collection = codebuddy_local_access::load_collection();
    Ok(serde_json::json!({
        "groups": collection.model_groups,
        "keys": collection.all_api_keys(),
        "accountCatalogs": collection.account_model_catalogs,
    }))
}

#[tauri::command]
pub async fn codebuddy_local_access_set_account_free_models(
    account_id: String,
    platform: String,
    always_free_models: Vec<String>,
    night_only_free_models: Vec<String>,
) -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = codebuddy_local_access::load_collection();
    let platform = match platform.as_str() {
        "codebuddyCn" => CodebuddyLocalAccessPlatform::CodebuddyCn,
        "codebuddy" => CodebuddyLocalAccessPlatform::Codebuddy,
        _ => CodebuddyLocalAccessPlatform::Workbuddy,
    };
    let always: Vec<String> = always_free_models
        .into_iter()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    let night: Vec<String> = night_only_free_models
        .into_iter()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    let mut found = false;
    for entry in &mut collection.accounts {
        if entry.account_id == account_id && entry.platform == platform {
            entry.always_free_models = always.clone();
            entry.night_only_free_models = night.clone();
            found = true;
        }
    }
    if !found {
        return Err(format!("账号未加入服务: {}/{}", platform.as_str(), account_id));
    }
    codebuddy_local_access::apply_collection(collection).await
}

#[tauri::command]
pub async fn codebuddy_local_access_set_night_free(
    enabled: bool,
    models: Vec<String>,
) -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = codebuddy_local_access::load_collection();
    collection.night_free_enabled = enabled;
    collection.night_free_models = models
        .into_iter()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    codebuddy_local_access::apply_collection(collection).await
}

#[tauri::command]
pub async fn codebuddy_local_access_set_disabled_models(
    disabled_models: Vec<String>,
) -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = codebuddy_local_access::load_collection();
    collection.disabled_models = disabled_models
        .into_iter()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    codebuddy_local_access::apply_collection(collection).await
}

#[tauri::command]
pub async fn codebuddy_local_access_get_model_catalog() -> Result<serde_json::Value, String> {
    Ok(codebuddy_local_access::model_catalog_payload().await)
}

#[tauri::command]
pub async fn codebuddy_local_access_query_request_logs_by_key(
    api_key: Option<String>,
    offset: Option<u32>,
    limit: Option<u32>,
) -> Result<serde_json::Value, String> {
    codebuddy_local_access::query_runtime_requests_filtered(api_key, offset, limit).await
}

#[tauri::command]
pub async fn codebuddy_local_access_api_key_stats() -> Result<serde_json::Value, String> {
    Ok(codebuddy_local_access::api_key_stats_payload().await)
}

#[tauri::command]
pub async fn codebuddy_local_access_clear_sticky_sessions() -> Result<serde_json::Value, String> {
    codebuddy_local_access::clear_sticky_sessions().await
}
