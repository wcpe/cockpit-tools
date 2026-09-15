// CodeBuddy / WorkBuddy 本地 API 服务（反代）命令。
// 与 Codex API Service 命令保持同一契约风格：Result<T, String> + camelCase 参数。

use crate::models::codebuddy_local_access::{
    CodebuddyFetchModelsResult, CodebuddyLocalAccessAccountRef, CodebuddyLocalAccessState,
    CodebuddyLocalAccessTestResult, CodebuddyProbeResult,
};
use crate::modules::codebuddy_local_access;

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
    codebuddy_local_access::apply_collection(collection).await
}

/// Start the service with the persisted collection (regenerating the manifest).
#[tauri::command]
pub async fn codebuddy_local_access_start() -> Result<CodebuddyLocalAccessState, String> {
    let mut collection = codebuddy_local_access::load_collection();
    collection.enabled = true;
    codebuddy_local_access::apply_collection(collection).await
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
