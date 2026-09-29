import { invoke } from "@tauri-apps/api/core";
import type {
  CodebuddyFetchModelsResult,
  CodebuddyLocalAccessSavePayload,
  CodebuddyLocalAccessState,
  CodebuddyLocalAccessTestResult,
  CodebuddyModelUsageRow,
  CodebuddyProbeResult,
  CodebuddyRequestRecord,
  CodebuddyRuntimeStatus,
} from "../types/codebuddyLocalAccess";

export async function getCodebuddyLocalAccessState(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_get_state");
}

export async function saveCodebuddyLocalAccess(
  payload: CodebuddyLocalAccessSavePayload,
): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_save", {
    ...(payload as Record<string, unknown>),
  });
}

/** 更新附加密钥（改名/增删），不重启 sidecar。 */
export async function saveCodebuddyClientKeys(
  clientKeys: Array<{ id: string; label: string; key: string; enabled: boolean; modelGroupId?: string | null }>,
): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_save_client_keys", { clientKeys });
}

/** 统一保存全部 API Key（主密钥与附加密钥同构）。 */
export async function saveCodebuddyApiKeys(
  clientKeys: Array<{ id: string; label: string; key: string; enabled: boolean; modelGroupId?: string | null }>,
): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_save_api_keys", { clientKeys });
}

/** 保存模型分组（账号集合 + 可选模型列表）。 */
export async function setCodebuddyModelGroups(groups: Array<{
  id?: string;
  name: string;
  accountIds?: string[];
  accountKeys?: string[];
  modelIds?: string[];
  kind?: string | null;
}>): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_set_model_groups", { modelGroups: groups });
}

/** 读取模型分组 / 密钥绑定 / 按号模型目录。 */
export async function getCodebuddyModelGroups(): Promise<{
  groups?: Array<Record<string, unknown>>;
  keys?: Array<Record<string, unknown>>;
  accountCatalogs?: Array<Record<string, unknown>>;
}> {
  return await invoke("codebuddy_local_access_get_model_groups");
}

export async function startCodebuddyLocalAccess(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_start");
}

export async function stopCodebuddyLocalAccess(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_stop");
}

export async function restartCodebuddyLocalAccess(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_restart");
}

export async function rotateCodebuddyLocalAccessApiKey(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_rotate_api_key");
}

export async function testCodebuddyLocalAccess(): Promise<CodebuddyLocalAccessTestResult> {
  return await invoke("codebuddy_local_access_test");
}

export async function getCodebuddyLocalAccessDefaultModels(): Promise<string[]> {
  return await invoke("codebuddy_local_access_default_models");
}

/** 禁用/启用模型（网关 /v1/models 与 chat 均拒绝已禁用模型）。 */
export async function setCodebuddyDisabledModels(disabledModels: string[]) {
  return await invoke("codebuddy_local_access_set_disabled_models", { disabledModels });
}

/** 夜间免费窗口：仅窗口内允许列表中的模型；窗外网关拒绝以免计费。 */
export async function setCodebuddyNightFree(enabled: boolean, models: string[]) {
  return await invoke("codebuddy_local_access_set_night_free", { enabled, models });
}

/** 账号级免费策略：always=全天免费模型；nightOnly=仅夜间免费模型。 */
export async function setCodebuddyAccountFreeModels(payload: {
  accountId: string;
  platform: string;
  alwaysFreeModels: string[];
  nightOnlyFreeModels: string[];
}) {
  return await invoke("codebuddy_local_access_set_account_free_models", payload);
}

/** 模型目录：企业参数缓存 + 禁用列表 + sidecar 运行态（免费/消耗）。 */
export async function getCodebuddyModelCatalog(): Promise<{
  models: Array<Record<string, unknown>>;
  disabledModels?: string[];
  runtime?: Record<string, unknown> | null;
  collectionModelIds?: string[];
}> {
  return await invoke("codebuddy_local_access_get_model_catalog");
}

/** 按 API Key 查询请求流水（sidecar 运行时）。 */
export async function getCodebuddyRequestLogsByKey(
  apiKey: string | null,
  offset = 0,
  limit = 20,
): Promise<{
  records: Array<Record<string, unknown>>;
  total: number;
  apiKeyStats?: Record<string, unknown>;
  modelUsage?: unknown[];
}> {
  return await invoke("codebuddy_local_access_query_request_logs_by_key", {
    apiKey,
    offset,
    limit,
  });
}

/** 各 API Key 调用量汇总。 */
export async function getCodebuddyApiKeyStats(): Promise<{
  apiKeyStats: Record<
    string,
    { ok?: number; fail?: number; credit?: number; tokens?: number; requests?: number; apiKeyLabel?: string }
  >;
  clientKeys?: Array<{ id: string; label: string; key: string; enabled: boolean }>;
  primaryKeyLabel?: string;
}> {
  return await invoke("codebuddy_local_access_api_key_stats");
}

/** 清除全部会话粘性绑定（换号/测缓存前重置）。 */
export async function clearCodebuddyStickySessions(): Promise<{ cleared: number }> {
  return await invoke("codebuddy_local_access_clear_sticky_sessions");
}

/** 直连上游企业模型接口，拉取所选账号真实可用的模型。 */
export async function fetchCodebuddyModels(): Promise<CodebuddyFetchModelsResult> {
  return await invoke("codebuddy_local_access_fetch_models");
}

/** 经本地服务发起一次真实对话，探测模型是否响应。 */
export async function probeCodebuddyChat(
  model: string,
  prompt?: string,
): Promise<CodebuddyProbeResult> {
  return await invoke("codebuddy_local_access_probe_chat", { model, prompt: prompt ?? null });
}

/** 从运行中的 sidecar 拉取冷却台账与请求流水。 */
export async function getCodebuddyRuntimeStatus(): Promise<CodebuddyRuntimeStatus> {
  return await invoke("codebuddy_local_access_runtime_status");
}

/** 分页拉取请求流水（offset/limit，来自运行中 sidecar 的 JSON 环缓）。 */
export async function getCodebuddyRuntimeRequests(
  offset = 0,
  limit = 20,
): Promise<{
  total: number;
  offset: number;
  limit: number;
  records: CodebuddyRequestRecord[];
  modelUsage?: CodebuddyModelUsageRow[];
}> {
  return await invoke("codebuddy_local_access_runtime_requests", { offset, limit });
}

/** 分页拉取 SQLite 持久化请求历史（服务停止后仍可查询）。 */
export async function getCodebuddyRequestLogs(
  offset = 0,
  limit = 20,
): Promise<{
  records: CodebuddyRequestRecord[];
  total: number;
  offset: number;
  limit: number;
  modelUsage?: CodebuddyModelUsageRow[];
}> {
  // SQLite full history — not the 500-row sidecar ring buffer.
  return await invoke("codebuddy_local_access_query_request_logs", { offset, limit });
}

/** 查询 SQLite 聚合用量（按模型 / 账号）。 */
export async function getCodebuddyUsageStats(): Promise<{
  modelUsage: CodebuddyModelUsageRow[];
  accountStats: Array<{
    accountId: string;
    model: string;
    ok: number;
    fail: number;
    credit: number;
  }>;
  totalRequests: number;
  okRequests: number;
  failRequests: number;
  totalTokens: number;
  totalCredit: number;
}> {
  return await invoke("codebuddy_local_access_query_usage_stats");
}

/** 清空 SQLite 请求历史库。 */
export async function clearCodebuddyRequestLogs(): Promise<void> {
  await invoke("codebuddy_local_access_clear_request_logs");
}
