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
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CodebuddyLocalAccessCollection {
    #[serde(default)]
    pub enabled: bool,
    #[serde(default = "default_codebuddy_local_access_port")]
    pub port: u16,
    #[serde(default)]
    pub api_key: String,
    #[serde(default)]
    pub access_scope: CodebuddyLocalAccessScope,
    /// Emit `reasoning_content` in streamed chunks. Off by default because it is
    /// a non-standard field for strict OpenAI clients.
    #[serde(default)]
    pub include_reasoning: bool,
    #[serde(default)]
    pub routing_strategy: CodebuddyLocalAccessRoutingStrategy,
    #[serde(default)]
    pub accounts: Vec<CodebuddyLocalAccessAccountRef>,
    /// Empty means "use the built-in preset for each upstream".
    #[serde(default)]
    pub model_ids: Vec<String>,
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
            access_scope: CodebuddyLocalAccessScope::default(),
            include_reasoning: false,
            routing_strategy: CodebuddyLocalAccessRoutingStrategy::default(),
            accounts: Vec::new(),
            model_ids: Vec::new(),
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
    pub raw: Option<serde_json::Value>,
}
