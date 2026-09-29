export type CodebuddyLocalAccessPlatform = "workbuddy" | "codebuddyCn" | "codebuddy";

export type CodebuddyLocalAccessScope = "localhost" | "lan";

export type CodebuddyLocalAccessRoutingStrategy = "roundRobin" | "random";

export interface CodebuddyLocalAccessAccountRef {
  platform: CodebuddyLocalAccessPlatform;
  accountId: string;
  label?: string | null;
  alwaysFreeModels?: string[];
  nightOnlyFreeModels?: string[];
}

export interface CodebuddyClientKey {
  id: string;
  label: string;
  key: string;
  enabled: boolean;
  /** Bind this key to a model group (accounts + optional model allow-list). */
  modelGroupId?: string | null;
}

export interface CodebuddyModelGroup {
  id: string;
  name: string;
  accountIds?: string[];
  accountKeys?: string[];
  modelIds?: string[];
  kind?: string | null;
}

export interface CodebuddyAccountModelCatalog {
  accountId: string;
  platform?: string;
  label?: string;
  models?: Array<Record<string, unknown>>;
}

export interface CodebuddyModelCatalogRow {
  id: string;
  name?: string;
  contextLength?: number;
  maxOutputTokens?: number;
  credits?: string;
  creditsRate?: number;
  tags?: string[];
  vendor?: string | null;
  isDefault?: boolean;
  onlyReasoning?: boolean;
  maxAllowedSize?: number;
  efforts?: string[];
  supportsImages?: boolean;
  supportsReasoning?: boolean;
  supportsToolCall?: boolean;
  cli?: boolean;
  disabled?: boolean;
  freeObserved?: boolean | null;
  costPer1k?: number | null;
  costTier?: number | null;
  costSamples?: number;
  accountsAlwaysFree?: string[];
  accountsNightFree?: string[];
  freePolicySummary?: string;
  requests?: number;
  totalTokens?: number;
  totalCredit?: number;
  source?: string;
  description?: string;
}

export interface CodebuddyLocalAccessCollection {
  enabled: boolean;
  port: number;
  apiKey: string;
  clientKeys?: CodebuddyClientKey[];
  accessScope: CodebuddyLocalAccessScope;
  includeReasoning: boolean;
  routingStrategy: CodebuddyLocalAccessRoutingStrategy;
  accounts: CodebuddyLocalAccessAccountRef[];
  modelIds: string[];
  disabledModels?: string[];
  modelCatalog?: Array<Record<string, unknown>>;
  nightFreeEnabled?: boolean;
  nightFreeModels?: string[];
  modelGroups?: CodebuddyModelGroup[];
  accountModelCatalogs?: CodebuddyAccountModelCatalog[];
}

export interface CodebuddyLocalAccessAccountOption {
  platform: CodebuddyLocalAccessPlatform;
  platformName: string;
  accountId: string;
  label: string;
  selected: boolean;
  tokenAvailable: boolean;
  expiresAtMs?: number | null;
  creditsRemain?: number | null;
  creditsSize?: number | null;
}

export interface CodebuddyLocalAccessState {
  collection: CodebuddyLocalAccessCollection;
  running: boolean;
  baseUrl?: string | null;
  lanBaseUrl?: string | null;
  lastError?: string | null;
  availableAccounts: CodebuddyLocalAccessAccountOption[];
  modelIds: string[];
}

export interface CodebuddyLocalAccessTestResult {
  ok: boolean;
  status?: number | null;
  message: string;
  latencyMs?: number | null;
  content?: string | null;
}

export interface CodebuddyLocalAccessSavePayload {
  enabled?: boolean;
  port?: number;
  accessScope?: CodebuddyLocalAccessScope;
  includeReasoning?: boolean;
  routingStrategy?: CodebuddyLocalAccessRoutingStrategy;
  accounts?: CodebuddyLocalAccessAccountRef[];
  modelIds?: string[];
  clientKeys?: CodebuddyClientKey[];
}

export interface CodebuddyModelInfo {
  id: string;
  name: string;
  contextLength: number;
  maxOutputTokens: number;
  supportsImages: boolean;
  supportsReasoning: boolean;
  supportsToolCall: boolean;
  efforts: string[];
  description?: string | null;
  /** 上游 `cli` agent 暴露（即网关真正可用）。 */
  cli: boolean;
  credits?: string | null;
  creditsRate?: number | null;
  tags?: string[];
  vendor?: string | null;
  isDefault?: boolean;
  onlyReasoning?: boolean;
  maxAllowedSize?: number;
}

export interface CodebuddyFetchModelsAccountResult {
  accountId: string;
  platform: CodebuddyLocalAccessPlatform;
  label: string;
  ok: boolean;
  message: string;
  count: number;
}

export interface CodebuddyFetchModelsResult {
  ok: boolean;
  message: string;
  models: CodebuddyModelInfo[];
  accounts: CodebuddyFetchModelsAccountResult[];
}

export interface CodebuddyProbeResult {
  ok: boolean;
  model: string;
  status?: number | null;
  message: string;
  latencyMs?: number | null;
  content?: string | null;
  reasoning?: string | null;
  totalTokens?: number | null;
}

export interface CodebuddyRequestRecord {
  id: string;
  timestamp: string;
  timestampUnixMs?: number | null;
  accountId?: string | null;
  accountLabel?: string | null;
  model: string;
  clientStream?: boolean;
  outcome: string;
  httpStatus?: number | null;
  latencyMs?: number | null;
  message?: string | null;
  reasonCode?: string | null;
  resetAt?: string | null;
  conversationRequestId?: string | null;
  promptTokens?: number | null;
  completionTokens?: number | null;
  totalTokens?: number | null;
  credit?: number | null;
  hasCredit?: boolean;
  maxTokens?: number | null;
  temperature?: number | null;
  topP?: number | null;
  messageCount?: number | null;
  systemChars?: number | null;
  toolCount?: number | null;
  toolChoice?: string | null;
  finishReason?: string | null;
  attempt?: number | null;
  degradedPrompt?: boolean;
  promptMode?: string | null;
  includeReasoning?: boolean;
  cachedTokens?: number | null;
  cacheWriteTokens?: number | null;
  reasoningTokens?: number | null;
  upstreamStream?: boolean;
  firstTokenMs?: number | null;
  totalMs?: number | null;
}

export interface CodebuddyRateLimitedModel {
  id?: string;
  accountId?: string;
  accountLabel?: string;
  model: string;
  until?: string | null;
  resetAt?: string | null;
  reason?: string | null;
}

export interface CodebuddyModelUsageRow {
  model: string;
  requests: number;
  inputTokens: number;
  cacheRead: number;
  cacheWrite: number;
  totalInput: number;
  outputTokens: number;
  cacheHitPct: number;
  totalTokens: number;
  credit?: number | null;
  lastAt?: string | null;
}

export interface CodebuddyPoolAccount {
  id: string;
  uid?: string;
  label?: string;
  realm?: string;
  credits?: number;
  creditsExpiring?: number;
  creditsKnown?: boolean;
  inFlight?: number;
  maxInFlight?: number;
  weight?: number;
  disabled?: boolean;
  disableReason?: string;
  wafUntil?: string | null;
  degradeUntil?: string | null;
  alwaysFreeModels?: string[];
  nightOnlyFreeModels?: string[];
  modelCosts?: Record<
    string,
    { costPer1k?: number; tier?: number; samples?: number; lastSeen?: string }
  >;
  modelCooldowns?: Array<{ model: string; until?: string }>;
}

export interface CodebuddyRuntimeStatus {
  promptMode?: string;
  rateLimitedModels?: CodebuddyRateLimitedModel[];
  recentRequests?: CodebuddyRequestRecord[];
  requestStats?: Record<
    string,
    { accountId: string; model: string; ok: number; fail: number; credit?: number }
  >;
  modelUsage?: CodebuddyModelUsageRow[];
  sessionAffinityCount?: number;
  accounts?: Array<Record<string, unknown>>;
  pool?: {
    accounts?: CodebuddyPoolAccount[];
    costExploreEvents?: number;
    wafIpActive?: boolean;
    wafIpUntil?: string | null;
  };
  modelCredits?: Record<string, string>;
  modelEfforts?: Record<string, string[]>;
  catalog?: CodebuddyModelCatalogRow[];
  costExplore?: {
    enabled?: boolean;
    interval?: string;
    defaultInterval?: string;
    env?: string;
    note?: string;
  };
  apiKeyStats?: Record<
    string,
    { ok?: number; fail?: number; credit?: number; tokens?: number; requests?: number; apiKeyLabel?: string }
  >;
  disabledModels?: string[];
  nightFree?: {
    enabled?: boolean;
    models?: string[];
    startHour?: number;
    endHour?: number;
    window?: string;
    inWindow?: boolean;
    nowCST?: string;
    behavior?: string;
  };
}
