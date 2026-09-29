use serde::{Deserialize, Serialize};

/// Which CodeBuddy-family product an account belongs to. All three speak the
/// same `/v2/chat/completions` gateway protocol but authenticate against
/// different hosts.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum CodebuddyLocalAccessPlatform {
    /// WorkBuddy (`copilot.tencent.com`).
    Workbuddy,
    /// CodeBuddy CN (`www.codebuddy.cn`).
    CodebuddyCn,
    /// CodeBuddy international (`www.codebuddy.ai`).
    Codebuddy,
}

impl CodebuddyLocalAccessPlatform {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Workbuddy => "workbuddy",
            Self::CodebuddyCn => "codebuddyCn",
            Self::Codebuddy => "codebuddy",
        }
    }

    /// Upstream gateway host used for `POST {base}/v2/chat/completions`.
    pub fn default_base_url(self) -> &'static str {
        match self {
            Self::Workbuddy => "https://copilot.tencent.com",
            Self::CodebuddyCn => "https://copilot.tencent.com",
            Self::Codebuddy => "https://www.codebuddy.ai",
        }
    }

    pub fn display_name(self) -> &'static str {
        match self {
            Self::Workbuddy => "WorkBuddy",
            Self::CodebuddyCn => "CodeBuddy CN",
            Self::Codebuddy => "CodeBuddy",
        }
    }
}

/// The platform services do not expose a discoverable model catalog to clients
/// (`GET /v3/config` rejects non-IDE User-Agents), so this preset mirrors the
/// `cli` agent entry of `GET /console/enterprises/personal/models` and the page
/// offers a live "拉取模型" refresh. The upstream list changes often — prefer
/// fetching it over relying on this constant.
pub const CODEBUDDY_DEFAULT_MODEL_IDS: &[&str] = &[
    "auto",
    "hy4-preview",
    "hy3",
    "hy3-x",
    "deepseek-v4.1-flash",
    "glm-5.3",
    "glm-5.3-flash",
    "glm-5.2",
    "glm-5.1",
    "glm-5v-turbo",
    "kimi-k3-1",
    "kimi-k2.8-preview",
    "kimi-k2.7",
    "kimi-k2.6",
    "minimax-m3",
    "deepseek-v4-pro",
];

/// One model as reported by the upstream enterprise catalog.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyModelInfo {
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub context_length: i64,
    #[serde(default)]
    pub max_output_tokens: i64,
    #[serde(default)]
    pub supports_images: bool,
    #[serde(default)]
    pub supports_reasoning: bool,
    #[serde(default)]
    pub supports_tool_call: bool,
    #[serde(default)]
    pub efforts: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    /// Exposed to the `cli` agent (i.e. callable through the gateway).
    #[serde(default)]
    pub cli: bool,
    /// Upstream credit rate string, e.g. "x0.00" / "x0.05" / "x0.29".
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credits: Option<String>,
    /// Parsed numeric rate when credits looks like xN.NN.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credits_rate: Option<f64>,
    /// Upstream tags, e.g. craft / badge:限时免费:#FF0000.
    #[serde(default)]
    pub tags: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub vendor: Option<String>,
    #[serde(default)]
    pub is_default: bool,
    #[serde(default)]
    pub only_reasoning: bool,
    #[serde(default)]
    pub max_allowed_size: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyFetchModelsAccountResult {
    pub account_id: String,
    pub platform: CodebuddyLocalAccessPlatform,
    pub label: String,
    pub ok: bool,
    pub message: String,
    pub count: usize,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyFetchModelsResult {
    pub ok: bool,
    pub message: String,
    /// Union of every account catalog, in `cli` order first.
    pub models: Vec<CodebuddyModelInfo>,
    pub accounts: Vec<CodebuddyFetchModelsAccountResult>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyProbeResult {
    pub ok: bool,
    pub model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub status: Option<u16>,
    pub message: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub latency_ms: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reasoning: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub total_tokens: Option<i64>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum CodebuddyLocalAccessScope {
    Localhost,
    Lan,
}

impl Default for CodebuddyLocalAccessScope {
    fn default() -> Self {
        Self::Localhost
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum CodebuddyLocalAccessRoutingStrategy {
    RoundRobin,
    Random,
}

impl Default for CodebuddyLocalAccessRoutingStrategy {
    fn default() -> Self {
        Self::RoundRobin
    }
}

/// One platform account selected into the service.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyLocalAccessAccountRef {
    pub platform: CodebuddyLocalAccessPlatform,
    pub account_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub label: Option<String>,
    /// Models free 24/7 on this account (e.g. hy4-preview).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub always_free_models: Vec<String>,
    /// Models free only in night window on this account.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub night_only_free_models: Vec<String>,
}

/// Unified client API key — primary and extra keys are the same shape.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyClientKey {
    pub id: String,
    #[serde(default)]
    pub label: String,
    pub key: String,
    #[serde(default = "default_true")]
    pub enabled: bool,
    /// Optional bind: only route to this model group's accounts/models.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model_group_id: Option<String>,
}

fn default_true() -> bool {
    true
}

/// Model group: a named set of accounts (and optionally models) that a key can bind to.
/// Used to build 免费模型分组 / 付费模型分组 etc.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyModelGroup {
    pub id: String,
    #[serde(default)]
    pub name: String,
    /// Account ids in this group (platform optional via accountKeys).
    #[serde(default)]
    pub account_ids: Vec<String>,
    /// Optional platform-aware membership: "platform:accountId".
    #[serde(default)]
    pub account_keys: Vec<String>,
    /// Explicit model allow-list for this group (empty = all models of member accounts).
    #[serde(default)]
    pub model_ids: Vec<String>,
    /// Hint only: free / paid / custom (UI grouping).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub kind: Option<String>,
}

impl CodebuddyModelGroup {
    pub fn contains_account(&self, platform: CodebuddyLocalAccessPlatform, account_id: &str) -> bool {
        if self.account_ids.iter().any(|id| id == account_id) {
            return true;
        }
        let key = format!("{}:{}", platform.as_str(), account_id);
        self.account_keys.iter().any(|k| k == &key)
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyAccountModelCatalog {
    #[serde(default)]
    pub account_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub platform: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub label: Option<String>,
    #[serde(default)]
    pub models: Vec<serde_json::Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyLocalAccessCollection {
    #[serde(default)]
    pub enabled: bool,
    #[serde(default = "default_codebuddy_local_access_port")]
    pub port: u16,
    /// Legacy single key; kept for compat — merged into all_api_keys().
    #[serde(default)]
    pub api_key: String,
    /// Unified API keys (all equal; optional modelGroupId binding).
    #[serde(default)]
    pub client_keys: Vec<CodebuddyClientKey>,
    #[serde(default)]
    pub access_scope: CodebuddyLocalAccessScope,
    #[serde(default)]
    pub include_reasoning: bool,
    #[serde(default)]
    pub routing_strategy: CodebuddyLocalAccessRoutingStrategy,
    #[serde(default)]
    pub accounts: Vec<CodebuddyLocalAccessAccountRef>,
    #[serde(default)]
    pub model_ids: Vec<String>,
    #[serde(default)]
    pub disabled_models: Vec<String>,
    #[serde(default)]
    pub model_catalog: Vec<serde_json::Value>,
    #[serde(default)]
    pub night_free_enabled: bool,
    #[serde(default)]
    pub night_free_models: Vec<String>,
    #[serde(default)]
    pub model_groups: Vec<CodebuddyModelGroup>,
    #[serde(default)]
    pub account_model_catalogs: Vec<CodebuddyAccountModelCatalog>,
}

impl CodebuddyLocalAccessCollection {
    /// All API keys as one list (legacy api_key merged in if missing).
    pub fn all_api_keys(&self) -> Vec<CodebuddyClientKey> {
        let mut keys = self.client_keys.clone();
        let primary = self.api_key.trim();
        if !primary.is_empty() && !keys.iter().any(|k| k.key.trim() == primary) {
            keys.insert(
                0,
                CodebuddyClientKey {
                    id: "default".to_string(),
                    label: "默认".to_string(),
                    key: primary.to_string(),
                    enabled: true,
                    model_group_id: None,
                },
            );
        }
        keys
    }

    pub fn model_group_by_id(&self, id: &str) -> Option<&CodebuddyModelGroup> {
        self.model_groups.iter().find(|g| g.id == id)
    }
}

fn default_codebuddy_local_access_port() -> u16 {
    8318
}

impl Default for CodebuddyLocalAccessCollection {
    fn default() -> Self {
        Self {
            enabled: false,
            port: default_codebuddy_local_access_port(),
            api_key: String::new(),
            client_keys: Vec::new(),
            access_scope: CodebuddyLocalAccessScope::default(),
            include_reasoning: false,
            routing_strategy: CodebuddyLocalAccessRoutingStrategy::default(),
            accounts: Vec::new(),
            model_ids: Vec::new(),
            disabled_models: Vec::new(),
            model_catalog: Vec::new(),
            night_free_enabled: false,
            night_free_models: Vec::new(),
            model_groups: Vec::new(),
            account_model_catalogs: Vec::new(),
        }
    }
}

/// A selectable account plus whether the service can currently use it.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyLocalAccessAccountOption {
    pub platform: CodebuddyLocalAccessPlatform,
    pub platform_name: String,
    pub account_id: String,
    pub label: String,
    pub selected: bool,
    pub token_available: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub expires_at_ms: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credits_remain: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credits_size: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyLocalAccessState {
    pub collection: CodebuddyLocalAccessCollection,
    pub running: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub base_url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub lan_base_url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_error: Option<String>,
    pub available_accounts: Vec<CodebuddyLocalAccessAccountOption>,
    pub model_ids: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyLocalAccessTestResult {
    pub ok: bool,
    pub status: Option<u16>,
    pub message: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub latency_ms: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyRateLimitedModel {
    #[serde(default)]
    pub model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub until: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reset_at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyRequestRecord {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub timestamp: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub timestamp_unix_ms: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub account_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub account_label: Option<String>,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub client_stream: bool,
    #[serde(default)]
    pub outcome: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub http_status: Option<i32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub latency_ms: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason_code: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reset_at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub conversation_request_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prompt_tokens: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub completion_tokens: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub total_tokens: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credit: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub has_credit: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub max_tokens: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub temperature: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub top_p: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message_count: Option<i32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub system_chars: Option<i32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool_count: Option<i32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool_choice: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub finish_reason: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub attempt: Option<i32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub degraded_prompt: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prompt_mode: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub include_reasoning: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cached_tokens: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cache_write_tokens: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reasoning_tokens: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub upstream_stream: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub first_token_ms: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub total_ms: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyModelUsageRow {
    pub model: String,
    #[serde(default)]
    pub requests: i64,
    #[serde(default)]
    pub input_tokens: i64,
    #[serde(default)]
    pub cache_read: i64,
    #[serde(default)]
    pub cache_write: i64,
    #[serde(default)]
    pub total_input: i64,
    #[serde(default)]
    pub output_tokens: i64,
    #[serde(default)]
    pub cache_hit_pct: f64,
    #[serde(default)]
    pub total_tokens: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credit: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_at: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyAccountModelStats {
    #[serde(default)]
    pub account_id: String,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub ok: i64,
    #[serde(default)]
    pub fail: i64,
    #[serde(default)]
    pub credit: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyRequestLogPage {
    pub records: Vec<CodebuddyRequestRecord>,
    pub total: u64,
    pub offset: u32,
    pub limit: u32,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model_usage: Option<Vec<CodebuddyModelUsageRow>>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyUsageStats {
    pub model_usage: Vec<CodebuddyModelUsageRow>,
    pub account_stats: Vec<CodebuddyAccountModelStats>,
    pub total_requests: u64,
    pub ok_requests: u64,
    pub fail_requests: u64,
    pub total_tokens: i64,
    pub total_credit: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyRuntimeStatus {
    #[serde(default)]
    pub prompt_mode: String,
    #[serde(default)]
    pub rate_limited_models: Vec<serde_json::Value>,
    #[serde(default)]
    pub recent_requests: Vec<CodebuddyRequestRecord>,
    #[serde(default)]
    pub request_stats: serde_json::Value,
    #[serde(default)]
    pub accounts: Vec<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pool: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model_credits: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model_efforts: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model_usage: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub session_affinity_count: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub raw: Option<serde_json::Value>,
}
