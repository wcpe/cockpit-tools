export type CodebuddyLocalAccessPlatform = "workbuddy" | "codebuddyCn" | "codebuddy";

export type CodebuddyLocalAccessScope = "localhost" | "lan";

export type CodebuddyLocalAccessRoutingStrategy = "roundRobin" | "random";

export interface CodebuddyLocalAccessAccountRef {
  platform: CodebuddyLocalAccessPlatform;
  accountId: string;
  label?: string | null;
}

export interface CodebuddyLocalAccessCollection {
  enabled: boolean;
  port: number;
  apiKey: string;
  accessScope: CodebuddyLocalAccessScope;
  includeReasoning: boolean;
  routingStrategy: CodebuddyLocalAccessRoutingStrategy;
  accounts: CodebuddyLocalAccessAccountRef[];
  modelIds: string[];
}

export interface CodebuddyLocalAccessAccountOption {
  platform: CodebuddyLocalAccessPlatform;
  platformName: string;
  accountId: string;
  label: string;
  selected: boolean;
  tokenAvailable: boolean;
  expiresAtMs?: number | null;
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
