import { Fragment, useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Activity,
  Database,
  Download,
  KeyRound,
  LayoutDashboard,
  ListOrdered,
  Play,
  RefreshCw,
  Square,
  Users,
  Wand2,
  X,
  Zap,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import * as service from "../services/codebuddyLocalAccessService";
import {
  findGroupByPlatform,
  resolveGroupChildName,
  usePlatformLayoutStore,
} from "../stores/usePlatformLayoutStore";
import { getPlatformLabel } from "../utils/platformMeta";
import { PlatformGroupSwitcher } from "../components/platform/PlatformGroupSwitcher";
import type {
  CodebuddyLocalAccessAccountOption,
  CodebuddyLocalAccessAccountRef,
  CodebuddyLocalAccessPlatform,
  CodebuddyLocalAccessScope,
  CodebuddyLocalAccessState,
  CodebuddyLocalAccessTestResult,
  CodebuddyModelCatalogRow,
  CodebuddyModelGroup,
  CodebuddyModelInfo,
  CodebuddyModelUsageRow,
  CodebuddyProbeResult,
  CodebuddyRequestRecord,
  CodebuddyRuntimeStatus,
} from "../types/codebuddyLocalAccess";
import "./CodebuddyApiServicePage.css";

const PLATFORM_ORDER: CodebuddyLocalAccessPlatform[] = ["workbuddy", "codebuddyCn", "codebuddy"];

function groupAccounts(options: CodebuddyLocalAccessAccountOption[]) {
  const groups = new Map<CodebuddyLocalAccessPlatform, CodebuddyLocalAccessAccountOption[]>();
  for (const option of options) {
    const list = groups.get(option.platform) ?? [];
    list.push(option);
    groups.set(option.platform, list);
  }
  return PLATFORM_ORDER.filter((platform) => groups.has(platform)).map((platform) => ({
    platform,
    name: groups.get(platform)?.[0]?.platformName ?? platform,
    accounts: groups.get(platform) ?? [],
  }));
}

function formatExpiry(expiresAtMs?: number | null): string {
  if (!expiresAtMs || expiresAtMs <= 0) {
    return "有效期未知";
  }
  const date = new Date(expiresAtMs);
  if (Number.isNaN(date.getTime())) {
    return "有效期未知";
  }
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(
    date.getDate(),
  ).padStart(2, "0")} 过期`;
}

function formatShortTs(value?: string | null): string {
  if (!value) {
    return "—";
  }
  // Prefer compact local time when parseable: MM-DD HH:mm:ss
  const date = new Date(value);
  if (!Number.isNaN(date.getTime())) {
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(
      date.getMinutes(),
    )}:${pad(date.getSeconds())}`;
  }
  return value.length > 19 ? `${value.slice(0, 19)}` : value;
}

function formatTokenCount(n?: number | null): string {
  if (n == null || !Number.isFinite(n) || n <= 0) {
    return "0";
  }
  if (n >= 1_000_000) {
    return `${(n / 1_000_000).toFixed(2)}M`;
  }
  if (n >= 1_000) {
    return `${(n / 1_000).toFixed(2)}K`;
  }
  return String(n);
}

function formatCredit(value?: number | null): string {
  if (value == null || !Number.isFinite(value)) {
    return "—";
  }
  // Keep enough precision for small per-call credits without noise.
  if (Math.abs(value) >= 1) {
    return String(Math.round(value * 100) / 100);
  }
  return value.toFixed(4).replace(/0+$/, "").replace(/\.$/, "");
}

function formatTs(value?: string | null): string {
  if (!value) {
    return "—";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleString();
}

function outcomeClass(outcome: string): string {
  switch (outcome) {
    case "ok":
      return "cblas-ok";
    case "rate_limited":
    case "quota":
      return "cblas-error";
    case "cooling":
      return "cblas-warn";
    default:
      return "cblas-error";
  }
}

function outcomeLabel(outcome: string): string {
  switch (outcome) {
    case "ok":
      return "成功";
    case "rate_limited":
      return "限流";
    case "cooling":
      return "冷却中";
    case "quota":
      return "余额";
    case "auth":
      return "鉴权";
    case "content_blocked":
      return "内容拦截";
    case "error":
      return "失败";
    default:
      return outcome;
  }
}

/**
 * CodeBuddy / CodeBuddy CN / WorkBuddy 本地 API 服务页面。
 *
 * 服务把选中的订阅账号聚合为一个 OpenAI 兼容端点，第三方客户端用下方
 * Base URL + API Key 即可调用。
 */
export function CodebuddyApiServicePage() {
  const { t } = useTranslation();
  const { platformGroups } = usePlatformLayoutStore();
  const currentPlatformId = "codebuddy_api_service" as const;
  const currentGroup = useMemo(
    () => findGroupByPlatform(platformGroups, currentPlatformId),
    [platformGroups, currentPlatformId],
  );
  const switchOptions = useMemo(
    () =>
      (currentGroup ? currentGroup.platformIds : [currentPlatformId]).map((platformId) => ({
        platformId,
        label: currentGroup
          ? resolveGroupChildName(currentGroup, platformId, getPlatformLabel(platformId, t))
          : getPlatformLabel(platformId, t),
      })),
    [currentGroup, t],
  );
  const [state, setState] = useState<CodebuddyLocalAccessState | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<CodebuddyLocalAccessTestResult | null>(null);
  const [portDraft, setPortDraft] = useState("");
  const [copied, setCopied] = useState<string | null>(null);
  const [fetchedModels, setFetchedModels] = useState<CodebuddyModelInfo[]>([]);
  const [fetchingModels, setFetchingModels] = useState(false);
  const [modelsMessage, setModelsMessage] = useState<string | null>(null);
  const [probeOpen, setProbeOpen] = useState(false);
  const [probeModel, setProbeModel] = useState("");
  const [probePrompt, setProbePrompt] = useState("");
  const [probing, setProbing] = useState(false);
  const [probeResult, setProbeResult] = useState<CodebuddyProbeResult | null>(null);
  const [runtimeStatus, setRuntimeStatus] = useState<CodebuddyRuntimeStatus | null>(null);
  const [runtimeStatusLoading, setRuntimeStatusLoading] = useState(false);
  const [runtimeStatusError, setRuntimeStatusError] = useState<string | null>(null);
  const [runtimeView, setRuntimeView] = useState<"pool" | "ledger" | "requests" | "usage">("requests");
  const [pageTab, setPageTab] = useState<
    "overview" | "accounts" | "logs" | "models" | "keys" | "help"
  >("logs");
  const [modelGroups, setModelGroups] = useState<CodebuddyModelGroup[]>([]);
  const [logFilterAccount, setLogFilterAccount] = useState("");
  const [logFilterModel, setLogFilterModel] = useState("");
  const [logFilterOutcome, setLogFilterOutcome] = useState("");
  const [coolingRows, setCoolingRows] = useState<
    Array<{ accountId?: string; accountLabel?: string; model?: string; kind?: string; until?: string; reason?: string }>
  >([]);
  const [requestPage, setRequestPage] = useState(0);
  const [requestPageSize, setRequestPageSize] = useState(50);
  const [requestTotal, setRequestTotal] = useState(0);
  const [requestRecords, setRequestRecords] = useState<CodebuddyRequestRecord[]>([]);
  const [requestsLoading, setRequestsLoading] = useState(false);
  const [expandedRequestId, setExpandedRequestId] = useState<string | null>(null);
  const [modelUsage, setModelUsage] = useState<CodebuddyModelUsageRow[]>([]);
  const [apiKeyStats, setApiKeyStats] = useState<
    Record<string, { ok?: number; fail?: number; credit?: number; tokens?: number; requests?: number }>
  >({});
  const [apiKeyFilter, setApiKeyFilter] = useState<string>("");

  const refresh = useCallback(async () => {
    try {
      const next = await service.getCodebuddyLocalAccessState();
      setState(next);
      setPortDraft(String(next.collection.port));
      if (next.collection.modelGroups) {
        setModelGroups(next.collection.modelGroups);
      }
      if (next.lastError) {
        setError(next.lastError);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    void refresh();
    // Always pull durable logs so cache/requests are visible even before service start.
    void loadRequestPage(0, 20);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refresh]);

  const refreshRuntimeStatus = useCallback(async () => {
    if (!state?.running) {
      setRuntimeStatus(null);
      setRuntimeStatusError(null);
      // Durable usage view works even when the sidecar is offline.
      try {
        const stats = await service.getCodebuddyUsageStats();
        if (stats.modelUsage?.length) {
          setModelUsage(stats.modelUsage);
        }
      } catch {
        // ignore — empty usage is acceptable when DB is empty
      }
      return;
    }
    setRuntimeStatusLoading(true);
    setRuntimeStatusError(null);
    try {
      const next = await service.getCodebuddyRuntimeStatus();
      setRuntimeStatus(next);
      if (Array.isArray((next as { cooling?: unknown }).cooling)) {
        setCoolingRows(
          (next as { cooling?: Array<{ accountId?: string; accountLabel?: string; model?: string; kind?: string; until?: string; reason?: string }> }).cooling ?? [],
        );
      }
      if (next.modelUsage) {
        setModelUsage(next.modelUsage as CodebuddyModelUsageRow[]);
      }
      if (next.apiKeyStats) {
        setApiKeyStats(next.apiKeyStats);
      }
      try {
        const stats = await service.getCodebuddyUsageStats();
        if (stats.modelUsage?.length) {
          setModelUsage(stats.modelUsage);
        }
      } catch {
        // durable store optional enrichment
      }
    } catch (err) {
      setRuntimeStatusError(err instanceof Error ? err.message : String(err));
    } finally {
      setRuntimeStatusLoading(false);
    }
  }, [state?.running]);

  const loadRequestPage = useCallback(
    async (page: number, pageSize: number, apiKey?: string) => {
      setRequestsLoading(true);
      setRuntimeStatusError(null);
      try {
        const offset = Math.max(0, page) * pageSize;
        const key = (apiKey ?? apiKeyFilter) || null;
        // Always prefer SQLite full history (not the 500-row sidecar ring).
        let result: {
          records: CodebuddyRequestRecord[];
          total: number;
          modelUsage?: CodebuddyModelUsageRow[];
        };
        if (key && state?.running) {
          // Live filter by API key from sidecar when service is up.
          try {
            const live = await service.getCodebuddyRequestLogsByKey(key, offset, pageSize);
            result = {
              records: (live.records ?? []) as unknown as CodebuddyRequestRecord[],
              total: live.total ?? 0,
              modelUsage: live.modelUsage as unknown as CodebuddyModelUsageRow[] | undefined,
            };
            if (live.apiKeyStats) {
              setApiKeyStats(live.apiKeyStats as Record<string, { ok?: number; fail?: number; credit?: number; tokens?: number; requests?: number }>);
            }
          } catch {
            result = await service.getCodebuddyRequestLogs(offset, pageSize);
          }
        } else {
          result = await service.getCodebuddyRequestLogs(offset, pageSize);
        }
        setRequestRecords(result.records ?? []);
        setRequestTotal(result.total ?? 0);
        if (result.modelUsage?.length) {
          setModelUsage(result.modelUsage);
        } else {
          const stats = await service.getCodebuddyUsageStats();
          setModelUsage(stats.modelUsage ?? []);
        }
        setRequestPage(page);
        setRequestPageSize(pageSize);
      } catch (err) {
        setRuntimeStatusError(err instanceof Error ? err.message : String(err));
      } finally {
        setRequestsLoading(false);
      }
    },
    [apiKeyFilter, state?.running],
  );

  useEffect(() => {
    if (state?.running) {
      void refreshRuntimeStatus();
    } else {
      setRuntimeStatus(null);
    }
  }, [state?.running, refreshRuntimeStatus]);

  useEffect(() => {
    if (runtimeView === "requests" || runtimeView === "usage") {
      void loadRequestPage(0, requestPageSize);
    } else {
      void refreshRuntimeStatus();
    }
  }, [runtimeView, loadRequestPage, requestPageSize, refreshRuntimeStatus]);

  const run = useCallback(
    async (action: () => Promise<CodebuddyLocalAccessState>) => {
      setBusy(true);
      setError(null);
      try {
        const next = await action();
        setState(next);
        setPortDraft(String(next.collection.port));
        if (next.lastError) {
          setError(next.lastError);
        }
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy(false);
      }
    },
    [],
  );

  const collection = state?.collection;
  const groups = useMemo(
    () => groupAccounts(state?.availableAccounts ?? []),
    [state?.availableAccounts],
  );
  const flatAccounts = useMemo(() => groups.flatMap((g) => g.accounts), [groups]);

  const selectedRefs: CodebuddyLocalAccessAccountRef[] = collection?.accounts ?? [];
  const [lastAnchorIndex, setLastAnchorIndex] = useState<number | null>(null);

  const isSelected = (platform: CodebuddyLocalAccessPlatform, accountId: string) =>
    selectedRefs.some((entry) => entry.platform === platform && entry.accountId === accountId);

  const saveAccounts = (next: CodebuddyLocalAccessAccountRef[]) => {
    if (!collection) {
      return;
    }
    void run(() =>
      service.saveCodebuddyLocalAccess({ accounts: next, enabled: collection.enabled }),
    );
  };

  const refFromOption = (option: CodebuddyLocalAccessAccountOption): CodebuddyLocalAccessAccountRef => ({
    platform: option.platform,
    accountId: option.accountId,
    label: option.label,
  });

  const toggleAccount = (
    option: CodebuddyLocalAccessAccountOption,
    index: number,
    event?: { shiftKey?: boolean },
  ) => {
    if (!collection) {
      return;
    }
    const currentlySelected = isSelected(option.platform, option.accountId);
    if (event?.shiftKey && lastAnchorIndex != null) {
      const start = Math.min(lastAnchorIndex, index);
      const end = Math.max(lastAnchorIndex, index);
      const range = flatAccounts.slice(start, end + 1);
      const map = new Map(selectedRefs.map((r) => [`${r.platform}|${r.accountId}`, r]));
      // Shift 连选：把区间统一设为与锚点目标一致（本次点击的 checked 目标）。
      const select = !currentlySelected;
      for (const item of range) {
        const key = `${item.platform}|${item.accountId}`;
        if (select) {
          if (!map.has(key)) {
            map.set(key, refFromOption(item));
          }
        } else {
          map.delete(key);
        }
      }
      setLastAnchorIndex(index);
      saveAccounts(Array.from(map.values()));
      return;
    }
    setLastAnchorIndex(index);
    const next = currentlySelected
      ? selectedRefs.filter(
          (entry) => !(entry.platform === option.platform && entry.accountId === option.accountId),
        )
      : [...selectedRefs, refFromOption(option)];
    saveAccounts(next);
  };

  const selectAllAccounts = () => {
    if (!collection || flatAccounts.length === 0) {
      return;
    }
    saveAccounts(flatAccounts.map(refFromOption));
  };

  const clearSelectedAccounts = () => {
    if (!collection) {
      return;
    }
    saveAccounts([]);
  };

  const invertSelectedAccounts = () => {
    if (!collection) {
      return;
    }
    saveAccounts(
      flatAccounts
        .filter((option) => !isSelected(option.platform, option.accountId))
        .map(refFromOption),
    );
  };

  const selectGroupAccounts = (groupAccounts: CodebuddyLocalAccessAccountOption[]) => {
    if (!collection || groupAccounts.length === 0) {
      return;
    }
    const map = new Map(selectedRefs.map((r) => [`${r.platform}|${r.accountId}`, r]));
    for (const item of groupAccounts) {
      map.set(`${item.platform}|${item.accountId}`, refFromOption(item));
    }
    saveAccounts(Array.from(map.values()));
  };

  /** accountId → 该号当前生效的模型级限流（6004 等）。 */
  const cooldownsByAccount = useMemo(() => {
    const map = new Map<
      string,
      Array<{ model: string; until?: string | null; resetAt?: string | null; reason?: string | null }>
    >();
    for (const item of runtimeStatus?.rateLimitedModels ?? []) {
      const key = item.accountId || item.id;
      if (!key) continue;
      const list = map.get(key) ?? [];
      list.push({
        model: item.model,
        until: item.until,
        resetAt: item.resetAt,
        reason: item.reason,
      });
      map.set(key, list);
    }
    return map;
  }, [runtimeStatus?.rateLimitedModels]);

  const handleToggleService = () => {
    if (!collection) {
      return;
    }
    if (state?.running) {
      void run(() => service.stopCodebuddyLocalAccess());
      return;
    }
    void run(() => service.saveCodebuddyLocalAccess({ enabled: true }));
  };

  const handlePortCommit = () => {
    if (!collection) {
      return;
    }
    const port = Number.parseInt(portDraft, 10);
    if (!Number.isFinite(port) || port <= 0 || port > 65535) {
      setPortDraft(String(collection.port));
      setError("端口必须是 1-65535 之间的整数");
      return;
    }
    if (port === collection.port) {
      return;
    }
    void run(() => service.saveCodebuddyLocalAccess({ port, enabled: collection.enabled }));
  };

  const handleScopeChange = (scope: CodebuddyLocalAccessScope) => {
    if (!collection) {
      return;
    }
    void run(() => service.saveCodebuddyLocalAccess({ accessScope: scope, enabled: collection.enabled }));
  };

  const handleReasoningChange = (includeReasoning: boolean) => {
    if (!collection) {
      return;
    }
    void run(() =>
      service.saveCodebuddyLocalAccess({ includeReasoning, enabled: collection.enabled }),
    );
  };

  const handleRoutingChange = (routingStrategy: "roundRobin" | "random") => {
    if (!collection) {
      return;
    }
    void run(() =>
      service.saveCodebuddyLocalAccess({ routingStrategy, enabled: collection.enabled }),
    );
  };

  const copy = async (kind: string, value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(kind);
      window.setTimeout(() => setCopied(null), 1500);
    } catch {
      setError("复制失败，请手动选择文本");
    }
  };

  const handleTest = async () => {
    setBusy(true);
    setError(null);
    try {
      setTestResult(await service.testCodebuddyLocalAccess());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  /** 网关真正可用的模型：优先 `cli` 标记，没有标记时退回全部。 */
  const usableModelIds = useMemo(() => {
    const cliIds = fetchedModels.filter((model) => model.cli).map((model) => model.id);
    return cliIds.length > 0 ? cliIds : fetchedModels.map((model) => model.id);
  }, [fetchedModels]);

  const probeModelOptions = useMemo(() => {
    if (fetchedModels.length > 0) {
      return fetchedModels.map((model) => ({ id: model.id, label: model.name || model.id }));
    }
    return (state?.modelIds ?? []).map((id) => ({ id, label: id }));
  }, [fetchedModels, state?.modelIds]);

  const handleFetchModels = async () => {
    setFetchingModels(true);
    setModelsMessage(null);
    setError(null);
    try {
      const result = await service.fetchCodebuddyModels();
      setFetchedModels(result.models);
      const failures = result.accounts.filter((account) => !account.ok);
      const detail = failures.map((account) => `${account.label}: ${account.message}`).join('；');
      setModelsMessage(detail ? `${result.message}（${detail}）` : result.message);
      if (result.models.length > 0 && !probeModel) {
        setProbeModel(result.models[0].id);
      }
    } catch (err) {
      setModelsMessage(err instanceof Error ? err.message : String(err));
    } finally {
      setFetchingModels(false);
    }
  };

  const handleApplyFetchedModels = () => {
    if (!collection || usableModelIds.length === 0) {
      return;
    }
    void run(() =>
      service.saveCodebuddyLocalAccess({
        modelIds: usableModelIds,
        enabled: collection.enabled,
      }),
    );
    setModelsMessage(`已应用 ${usableModelIds.length} 个模型`);
  };

  const openProbe = () => {
    setProbeResult(null);
    if (!probeModel) {
      setProbeModel(probeModelOptions[0]?.id ?? "");
    }
    setProbeOpen(true);
  };

  const handleProbe = async () => {
    if (!probeModel) {
      return;
    }
    setProbing(true);
    setProbeResult(null);
    try {
      setProbeResult(await service.probeCodebuddyChat(probeModel, probePrompt));
    } catch (err) {
      setProbeResult({
        ok: false,
        model: probeModel,
        message: err instanceof Error ? err.message : String(err),
      });
    } finally {
      setProbing(false);
    }
  };

  return (
    <div className="cblas-page cblas-page--shell">
      <div className="page-top-strip">
        <div className="page-top-strip-left">
          <span className="page-top-strip-label">
            {t("settings.general.account", "Accounts")}
          </span>
        </div>
        <div className="page-top-strip-right-placeholder" aria-hidden="true" />
      </div>

      <div className="page-tabs-row page-tabs-center page-tabs-row-with-leading">
        <div className="page-tabs-leading">
          <PlatformGroupSwitcher
            currentPlatformId={currentPlatformId}
            currentLabel={
              currentGroup
                ? resolveGroupChildName(
                    currentGroup,
                    currentPlatformId,
                    getPlatformLabel(currentPlatformId, t),
                  )
                : getPlatformLabel(currentPlatformId, t)
            }
            options={switchOptions}
            currentGroupId={currentGroup?.id ?? null}
          />
        </div>
      </div>

      <main className="cblas-content">
      <header className="cblas-header cblas-hero">
        <div>
          <h2 className="cblas-title">WorkBuddy API 服务</h2>
          <p className="cblas-subtitle">
            把 WorkBuddy / CodeBuddy / CodeBuddy CN 订阅账号聚合成本机 OpenAI 兼容端点，
            供任意第三方客户端调用。请求经由本地 sidecar 转发到
            <span className="cblas-mono"> /v2/chat/completions</span>，Cockpit 负责令牌刷新。
          </p>
        </div>
        <div className="cblas-row" style={{ marginBottom: 0 }}>
          <span className={`cblas-badge ${state?.running ? "cblas-badge--on" : "cblas-badge--off"}`}>
            {state?.running ? "运行中" : "已停止"}
          </span>
        </div>
      </header>

      {error ? <div className="cblas-error">{error}</div> : null}

      <nav className="cblas-page-tabs" aria-label="API 服务功能区">
        {(
          [
            ["overview", "服务概览", <LayoutDashboard key="i" size={14} />],
            ["accounts", "账号池", <Users key="i" size={14} />],
            ["logs", "流水统计", <Activity key="i" size={14} />],
            ["models", "模型目录", <Database key="i" size={14} />],
            ["keys", "API 密钥", <KeyRound key="i" size={14} />],
            ["help", "接入说明", <ListOrdered key="i" size={14} />],
          ] as const
        ).map(([id, label, icon]) => (
          <button
            key={id}
            type="button"
            className={`cblas-page-tab ${pageTab === id ? "active" : ""}`}
            onClick={() => {
              setPageTab(id);
              if (id === "logs") {
                void refreshRuntimeStatus();
                void loadRequestPage(requestPage || 0, requestPageSize);
              }
            }}
          >
            {icon}
            {label}
          </button>
        ))}
      </nav>

      <div className="cblas-page-tab-body">
      {pageTab === "overview" ? (
        (() => {
          const usageTotals = modelUsage.reduce(
            (acc, row) => {
              acc.req += row.requests || 0;
              acc.input += row.totalInput || 0;
              acc.cache += row.cacheRead || 0;
              acc.write += row.cacheWrite || 0;
              acc.tokens += row.totalTokens || 0;
              acc.credit += row.credit || 0;
              return acc;
            },
            { req: 0, input: 0, cache: 0, write: 0, tokens: 0, credit: 0 },
          );
          const hitPct =
            usageTotals.input > 0 ? (usageTotals.cache / usageTotals.input) * 100 : null;
          const kpi = (
            label: string,
            value: ReactNode,
            foot: string,
            tone: "ok" | "warn" | "danger" | "info" | "muted" = "info",
            icon?: ReactNode,
          ) => (
            <div className={`cblas-kpi-card ${tone !== "info" ? `is-${tone}` : ""}`}>
              <div className="cblas-kpi-head">
                <span className="cblas-kpi-label">{label}</span>
                {icon ? <span className="cblas-kpi-icon">{icon}</span> : null}
              </div>
              <div className={`cblas-kpi-value ${typeof value === "string" && value.length > 12 ? "mono" : ""}`}>
                {value}
              </div>
              {foot ? <div className="cblas-kpi-foot">{foot}</div> : null}
            </div>
          );
          return (
            <>
              <div className="cblas-kpi-grid cblas-kpi-grid--wide">
                {kpi(
                  "服务状态",
                  state?.running ? "运行中" : "已停止",
                  state?.running ? `端口 ${collection?.port ?? "—"}` : "启动后可代理 /v1",
                  state?.running ? "ok" : "danger",
                  <Play size={14} />,
                )}
                {kpi(
                  "Base URL",
                  state?.baseUrl || "未启动",
                  state?.lanBaseUrl ? "已配置局域网地址" : "默认仅本机",
                  state?.baseUrl ? "info" : "muted",
                  <Zap size={14} />,
                )}
                {kpi(
                  "启用账号",
                  `${selectedRefs.length}/${flatAccounts.length}`,
                  `模型组 ${(collection?.modelGroups ?? []).length} · 密钥 ${(collection?.clientKeys ?? []).length + 1}`,
                  selectedRefs.length > 0 ? "ok" : "warn",
                  <Users size={14} />,
                )}
                {kpi(
                  "可用模型",
                  String((state?.modelIds ?? []).length),
                  `禁用 ${(collection?.disabledModels ?? []).length} · 目录 ${(runtimeStatus?.catalog ?? []).length || (state?.modelIds ?? []).length}`,
                  "info",
                  <Database size={14} />,
                )}
                {kpi(
                  "流水条数",
                  String(requestTotal || usageTotals.req),
                  "SQLite 持久化 · 重启不丢",
                  (requestTotal || usageTotals.req) > 0 ? "ok" : "muted",
                  <Activity size={14} />,
                )}
                {kpi(
                  "缓存命中率",
                  hitPct == null ? "—" : `${hitPct >= 99.5 ? "99+" : Math.round(hitPct)}%`,
                  `缓存读 ${formatTokenCount(usageTotals.cache)} / 写 ${formatTokenCount(usageTotals.write)}`,
                  hitPct != null && hitPct >= 40 ? "ok" : hitPct != null && hitPct > 0 ? "warn" : "muted",
                  <Activity size={14} />,
                )}
                {kpi(
                  "Token / Credit",
                  formatTokenCount(usageTotals.tokens),
                  `累计 credit ${formatCredit(usageTotals.credit)}`,
                  "info",
                  <ListOrdered size={14} />,
                )}
                {kpi(
                  "提示词 / 粘性",
                  runtimeStatus?.promptMode || "passthrough",
                  `会话粘性 ${runtimeStatus?.sessionAffinityCount ?? 0} · 同会话钉号吃缓存`,
                  "muted",
                  <Wand2 size={14} />,
                )}
              </div>

              {(() => {
                if (hitPct == null) return null;
                return (
                  <div className="cblas-card" style={{ marginTop: 4 }}>
                    <div className="cblas-card-head">
                      <h3>Prompt Cache</h3>
                      <span className="cblas-badge cblas-badge--on">
                        {hitPct >= 99.5 ? "99+" : Math.round(hitPct)}%
                      </span>
                    </div>
                    <div className="cblas-cache-bar" title={`cacheRead=${usageTotals.cache} totalInput=${usageTotals.input}`}>
                      <i style={{ width: `${Math.min(100, Math.max(0, hitPct))}%` }} />
                    </div>
                    <div className="cblas-kpi-foot" style={{ marginTop: 8 }}>
                      命中率高说明同会话粘性生效、上游前缀缓存吃到了；突然掉到 0 多半是换号或清了上下文。
                    </div>
                  </div>
                );
              })()}
            </>
          );
        })()
      ) : null}

      <section className="cblas-card" hidden={pageTab !== "keys"}>
        <div className="cblas-card-head">
          <h3>API 密钥</h3>
          <div className="cblas-row" style={{ marginBottom: 0 }}>
            <button
              type="button"
              className="cblas-button"
              disabled={!state?.running}
              onClick={() => {
                void run(async () => {
                  const r = await service.clearCodebuddyStickySessions();
                  setModelsMessage(`已清除 ${r.cleared ?? 0} 条会话粘性`);
                  await refreshRuntimeStatus();
                  return await service.getCodebuddyLocalAccessState();
                });
              }}
            >
              清除会话粘性
            </button>
            <button
              type="button"
              className="cblas-button cblas-button--primary"
              disabled={busy}
              onClick={() => {
                const next = [
                  ...(collection?.clientKeys ?? []),
                  {
                    id: `key_${Date.now()}`,
                    label: `Key ${(collection?.clientKeys?.length ?? 0) + 1}`,
                    key: `cbk-${Math.random().toString(36).slice(2, 10)}${Math.random().toString(36).slice(2, 10)}`,
                    enabled: true,
                    modelGroupId: null,
                  },
                ];
                void run(() => service.saveCodebuddyApiKeys(next));
              }}
            >
              新增密钥
            </button>
          </div>
        </div>
          <p className="cblas-note">
            所有密钥地位相同，不再区分主密钥/附加密钥。可为每个密钥绑定「模型分组」，只路由到分组内账号（及分组模型白名单）。
          </p>
          <table className="cblas-table cblas-key-stats">
            <thead>
              <tr>
                <th>名称</th>
                <th>密钥</th>
                <th>启用</th>
                <th>绑定模型分组</th>
                <th>调用</th>
                <th>Credit</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {(collection?.clientKeys ?? collection?.apiKey
                ? [
                    ...(collection?.apiKey
                      ? [
                          {
                            id: "default",
                            label: "默认",
                            key: collection?.apiKey ?? "",
                            enabled: true,
                            modelGroupId: null as string | null,
                          },
                        ]
                      : []),
                    ...(collection?.clientKeys ?? []),
                  ]
                : []
              ).map((item, index) => {
                const st =
                  apiKeyStats[item.id] ||
                  apiKeyStats[item.label] ||
                  Object.entries(apiKeyStats).find(
                    ([k]) => k.includes(item.id) || (item.label && k.includes(item.label)),
                  )?.[1] ||
                  {};
                return (
                  <tr key={item.id || index}>
                    <td>
                      <input
                        className="cblas-input"
                        style={{ width: 100 }}
                        value={item.label}
                        onChange={(e) => {
                          const list = [...(collection?.clientKeys ?? [])];
                          const i = list.findIndex((k) => k.id === item.id);
                          if (i >= 0) {
                            list[i] = { ...list[i], label: e.target.value };
                            void run(() => service.saveCodebuddyApiKeys(list));
                          }
                        }}
                      />
                    </td>
                    <td className="cblas-mono">{item.key}</td>
                    <td>
                      <input
                        type="checkbox"
                        checked={item.enabled}
                        onChange={(e) => {
                          const list = [...(collection?.clientKeys ?? [])];
                          const i = list.findIndex((k) => k.id === item.id);
                          if (i >= 0) {
                            list[i] = { ...list[i], enabled: e.target.checked };
                            void run(() => service.saveCodebuddyApiKeys(list));
                          }
                        }}
                      />
                    </td>
                    <td>
                      <select
                        className="cblas-select"
                        style={{ minWidth: 140 }}
                        value={item.modelGroupId ?? ""}
                        onChange={(e) => {
                          const list = [...(collection?.clientKeys ?? [])];
                          const i = list.findIndex((k) => k.id === item.id);
                          if (i >= 0) {
                            list[i] = {
                              ...list[i],
                              modelGroupId: e.target.value || null,
                            };
                            void run(() => service.saveCodebuddyApiKeys(list));
                          }
                        }}
                      >
                        <option value="">全部账号</option>
                        {modelGroups.map((g) => (
                          <option key={g.id} value={g.id}>
                            {g.name}
                          </option>
                        ))}
                      </select>
                    </td>
                    <td>{st.requests ?? "—"}</td>
                    <td>{formatCredit(st.credit)}</td>
                    <td className="row-actions">
                      <button
                        type="button"
                        className="cblas-button"
                        onClick={() => void copy(`k-${item.id}`, item.key)}
                      >
                        {copied === `k-${item.id}` ? "已复制" : "复制"}
                      </button>
                      {item.id !== "default" ? (
                        <button
                          type="button"
                          className="cblas-button"
                          onClick={() => {
                            const list = (collection?.clientKeys ?? []).filter(
                              (k) => k.id !== item.id,
                            );
                            void run(() => service.saveCodebuddyApiKeys(list));
                          }}
                        >
                          删除
                        </button>
                      ) : null}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </section>

      {pageTab === "models" ? (
        <section className="cblas-card">
          <div className="cblas-card-head">
            <h3>模型分组（按账号组成，供密钥绑定）</h3>
            <button
              type="button"
              className="cblas-button cblas-button--primary"
              disabled={busy}
              onClick={() => {
                const g: CodebuddyModelGroup = {
                  id: `grp_${Date.now()}`,
                  name: `分组 ${modelGroups.length + 1}`,
                  accountIds: [],
                  modelIds: [],
                  kind: "custom",
                };
                const next = [...modelGroups, g];
                setModelGroups(next);
                void run(() => service.setCodebuddyModelGroups(next));
              }}
            >
              新增分组
            </button>
          </div>
          <p className="cblas-note">
            建议：先「拉取企业模型（按号）」，再按某号 hy4-preview 全天免费等差异，把账号加入「免费分组 / 付费分组」，密钥绑定分组即可分流。
          </p>
          {modelGroups.map((g, gi) => (
            <div className="cblas-model-row" key={g.id}>
              <div className="cblas-row">
                <input
                  className="cblas-input"
                  style={{ width: 160 }}
                  value={g.name}
                  onChange={(e) => {
                    const next = [...modelGroups];
                    next[gi] = { ...g, name: e.target.value };
                    setModelGroups(next);
                  }}
                  onBlur={() => void run(() => service.setCodebuddyModelGroups(modelGroups))}
                />
                <select
                  className="cblas-select"
                  value={g.kind ?? "custom"}
                  onChange={(e) => {
                    const next = [...modelGroups];
                    next[gi] = { ...g, kind: e.target.value };
                    setModelGroups(next);
                    void run(() => service.setCodebuddyModelGroups(next));
                  }}
                >
                  <option value="free">免费</option>
                  <option value="paid">付费</option>
                  <option value="custom">自定义</option>
                </select>
                <button
                  type="button"
                  className="cblas-button"
                  onClick={() => {
                    const next = modelGroups.filter((x) => x.id !== g.id);
                    setModelGroups(next);
                    void run(() => service.setCodebuddyModelGroups(next));
                  }}
                >
                  删除分组
                </button>
              </div>
              <div className="cblas-row" style={{ alignItems: "flex-start" }}>
                <span className="cblas-label">账号</span>
                <div style={{ flex: 1, display: "flex", flexWrap: "wrap", gap: 6 }}>
                  {(state?.availableAccounts ?? []).map((acc) => {
                    const key = acc.accountId;
                    const on = (g.accountIds ?? []).includes(key);
                    return (
                      <label key={`${acc.platform}:${acc.accountId}`} style={{ fontSize: 12 }}>
                        <input
                          type="checkbox"
                          checked={on}
                          onChange={(e) => {
                            const set = new Set(g.accountIds ?? []);
                            if (e.target.checked) {
                              set.add(key);
                            } else {
                              set.delete(key);
                            }
                            const next = [...modelGroups];
                            next[gi] = { ...g, accountIds: Array.from(set) };
                            setModelGroups(next);
                            void run(() => service.setCodebuddyModelGroups(next));
                          }}
                        />{" "}
                        {acc.label || acc.accountId}
                        {acc.creditsRemain != null ? ` (${acc.creditsRemain})` : ""}
                      </label>
                    );
                  })}
                </div>
              </div>
              <div className="cblas-row" style={{ alignItems: "flex-start" }}>
                <span className="cblas-label">模型白名单</span>
                <input
                  className="cblas-input"
                  style={{ flex: 1, minWidth: 200 }}
                  placeholder="留空=分组内账号全部模型；可填 hy4-preview,glm-5.2"
                  value={(g.modelIds ?? []).join(",")}
                  onChange={(e) => {
                    const list = e.target.value.split(",").map((s) => s.trim()).filter(Boolean);
                    const next = [...modelGroups];
                    next[gi] = { ...g, modelIds: list };
                    setModelGroups(next);
                  }}
                  onBlur={() => void run(() => service.setCodebuddyModelGroups(modelGroups))}
                />
              </div>
            </div>
          ))}
          <div className="cblas-row" style={{ marginTop: 10 }}>
            <button
              type="button"
              className="cblas-button"
              disabled={busy}
              onClick={() => void run(() => service.setCodebuddyModelGroups(modelGroups))}
            >
              保存全部分组
            </button>
            <button
              type="button"
              className="cblas-button"
              disabled={fetchingModels || selectedRefs.length === 0}
              onClick={() => void handleFetchModels()}
            >
              <Download size={14} />
              <span style={{ marginLeft: 6 }}>拉取企业模型（按号）</span>
            </button>
          </div>
          {(runtimeStatus as { accountCatalogs?: Array<{ accountId: string; label?: string; credits?: Record<string, string>; alwaysFreeModels?: string[]; nightOnlyFreeModels?: string[]; models?: Array<{ id?: string }> }> })
            ?.accountCatalogs?.length ? (
            <div className="cblas-model-table" style={{ marginTop: 12 }}>
              <h3 style={{ margin: "0 0 8px", fontSize: 13 }}>按号模型目录（差异对照）</h3>
              {(
                runtimeStatus as {
                  accountCatalogs: Array<{
                    accountId: string;
                    label?: string;
                    credits?: Record<string, string>;
                    alwaysFreeModels?: string[];
                    nightOnlyFreeModels?: string[];
                    models?: Array<{ id?: string; credits?: string }>;
                  }>;
                }
              ).accountCatalogs!.map((acc) => (
                <div className="cblas-model-row" key={acc.accountId}>
                  <div className="cblas-model-main">
                    <span className="cblas-model-id">{acc.label || acc.accountId}</span>
                    {acc.alwaysFreeModels?.length ? (
                      <span className="cblas-model-tag is-free">
                        全天免费 {acc.alwaysFreeModels.join("/")}
                      </span>
                    ) : null}
                    {acc.nightOnlyFreeModels?.length ? (
                      <span className="cblas-model-tag is-paid">
                        仅夜间 {acc.nightOnlyFreeModels.join("/")}
                      </span>
                    ) : null}
                  </div>
                  <div className="cblas-model-meta">
                    {(acc.models ?? []).slice(0, 24).map((m) => {
                      const id = m.id ?? "";
                      const credits = acc.credits?.[id] || (m as { credits?: string }).credits || "—";
                      const always = (acc.alwaysFreeModels ?? []).includes(id);
                      return (
                        <span key={id} className="cblas-model-tag" style={{ marginRight: 4 }}>
                          {id}:{credits}
                          {always ? "·免费号" : ""}
                        </span>
                      );
                    })}
                  </div>
                </div>
              ))}
            </div>
          ) : null}
        </section>
      ) : null}

      <section className="cblas-card cblas-logs-card" hidden={pageTab !== "logs"}>
        <div className="cblas-card-head cblas-logs-head">
          <div>
            <h3>
              <Activity size={14} style={{ marginRight: 6, verticalAlign: "middle" }} />
              流水统计
            </h3>
            <div className="cblas-kpi-foot" style={{ marginTop: 4 }}>
              数据源：本地 SQLite（服务重启仍在） · 可判断是否吃到 prompt cache
            </div>
          </div>
          <button
            type="button"
            className="cblas-button"
            onClick={() => {
              void loadRequestPage(0, requestPageSize);
              void refreshRuntimeStatus();
            }}
            disabled={requestsLoading}
          >
            <RefreshCw size={14} className={requestsLoading ? "loading-spinner" : ""} />
            <span style={{ marginLeft: 6 }}>刷新</span>
          </button>
        </div>

        {(() => {
          const totals = modelUsage.reduce(
            (acc, row) => {
              acc.req += row.requests || 0;
              acc.input += row.totalInput || 0;
              acc.cache += row.cacheRead || 0;
              acc.write += row.cacheWrite || 0;
              acc.out += row.outputTokens || 0;
              acc.credit += row.credit || 0;
              return acc;
            },
            { req: 0, input: 0, cache: 0, write: 0, out: 0, credit: 0 },
          );
          const hitPct = totals.input > 0 ? (totals.cache / totals.input) * 100 : null;
          const tone = (v: "ok" | "warn" | "danger" | "info" | "muted") => (v === "info" ? "" : ` is-${v}`);
          return (
            <>
              <div className="cblas-kpi-grid cblas-kpi-grid--wide">
                <div className={`cblas-kpi-card${tone((requestTotal || totals.req) > 0 ? "ok" : "muted")}`}>
                  <div className="cblas-kpi-head">
                    <span className="cblas-kpi-label">流水条数</span>
                    <span className="cblas-kpi-icon"><Activity size={14} /></span>
                  </div>
                  <div className="cblas-kpi-value">{requestTotal || totals.req || 0}</div>
                  <div className="cblas-kpi-foot">SQLite 持久化</div>
                </div>
                <div className={`cblas-kpi-card${tone(hitPct != null && hitPct >= 40 ? "ok" : hitPct != null && hitPct > 0 ? "warn" : "muted")}`}>
                  <div className="cblas-kpi-head">
                    <span className="cblas-kpi-label">缓存命中率</span>
                    <span className="cblas-kpi-icon"><Zap size={14} /></span>
                  </div>
                  <div className="cblas-kpi-value">
                    {hitPct == null ? "—" : `${hitPct >= 99.5 ? "99+" : Math.round(hitPct)}%`}
                  </div>
                  <div className="cblas-cache-bar">
                    <i style={{ width: `${hitPct == null ? 0 : Math.min(100, Math.max(0, hitPct))}%` }} />
                  </div>
                </div>
                <div className="cblas-kpi-card">
                  <div className="cblas-kpi-head">
                    <span className="cblas-kpi-label">缓存读 / 写</span>
                    <span className="cblas-kpi-icon"><Database size={14} /></span>
                  </div>
                  <div className="cblas-kpi-value" style={{ fontSize: 18 }}>
                    {formatTokenCount(totals.cache)} / {formatTokenCount(totals.write)}
                  </div>
                  <div className="cblas-kpi-foot">读&gt;0 说明吃到了前缀缓存</div>
                </div>
                <div className="cblas-kpi-card">
                  <div className="cblas-kpi-head">
                    <span className="cblas-kpi-label">输入 / 输出</span>
                    <span className="cblas-kpi-icon"><ListOrdered size={14} /></span>
                  </div>
                  <div className="cblas-kpi-value" style={{ fontSize: 18 }}>
                    {formatTokenCount(totals.input)} / {formatTokenCount(totals.out)}
                  </div>
                  <div className="cblas-kpi-foot">总输入含缓存读</div>
                </div>
                <div className="cblas-kpi-card">
                  <div className="cblas-kpi-head">
                    <span className="cblas-kpi-label">累计 Credit</span>
                    <span className="cblas-kpi-icon"><Zap size={14} /></span>
                  </div>
                  <div className="cblas-kpi-value">{formatCredit(totals.credit)}</div>
                  <div className="cblas-kpi-foot">来自 usage.credit</div>
                </div>
                <div className={`cblas-kpi-card${tone(state?.running ? "ok" : "muted")}`}>
                  <div className="cblas-kpi-head">
                    <span className="cblas-kpi-label">会话粘性</span>
                    <span className="cblas-kpi-icon"><Users size={14} /></span>
                  </div>
                  <div className="cblas-kpi-value">{runtimeStatus?.sessionAffinityCount ?? 0}</div>
                  <div className="cblas-kpi-foot">同会话钉号，利于缓存</div>
                </div>
              </div>
              <div className="cblas-logs-toolbar">
                {(
                  [
                    ["requests", "请求流水"],
                    ["usage", "用量统计"],
                    ["pool", "选号池"],
                    ["ledger", "限流冷却"],
                  ] as const
                ).map(([id, label]) => (
                  <button
                    key={id}
                    type="button"
                    className={`cblas-button ${runtimeView === id ? "cblas-button--primary" : ""}`}
                    onClick={() => {
                      setRuntimeView(id);
                      if (id === "requests" || id === "usage") {
                        void loadRequestPage(0, requestPageSize);
                      } else {
                        void refreshRuntimeStatus();
                      }
                    }}
                    disabled={id === "pool" || id === "ledger" ? runtimeStatusLoading || requestsLoading : requestsLoading}
                  >
                    {label}
                  </button>
                ))}
              </div>
              {runtimeView === "requests" ? (
                <div className="cblas-logs-toolbar" style={{ marginTop: 4 }}>
                  <input
                    className="cblas-input"
                    placeholder="账号筛选"
                    style={{ width: 120 }}
                    value={logFilterAccount}
                    onChange={(e) => setLogFilterAccount(e.target.value.trim())}
                  />
                  <input
                    className="cblas-input"
                    placeholder="模型筛选"
                    style={{ width: 130 }}
                    value={logFilterModel}
                    onChange={(e) => setLogFilterModel(e.target.value.trim())}
                  />
                  <select
                    className="cblas-select"
                    style={{ width: 120 }}
                    value={logFilterOutcome}
                    onChange={(e) => setLogFilterOutcome(e.target.value)}
                  >
                    <option value="">全部结果</option>
                    <option value="ok">成功</option>
                    <option value="cooling">冷却</option>
                    <option value="rate_limited">限流</option>
                    <option value="client_aborted">中断</option>
                    <option value="error">失败</option>
                  </select>
                  <button
                    type="button"
                    className="cblas-button"
                    onClick={() => void loadRequestPage(0, requestPageSize)}
                  >
                    应用筛选
                  </button>
                  <span className="cblas-kpi-foot">最近在最上 · 全库可筛</span>
                </div>
              ) : null}
            </>
          );
        })()}
        {!state?.running && (runtimeView === "pool" || runtimeView === "ledger") ? (
          <p className="cblas-empty">
            服务未运行时仍可查看「请求流水 / 用量统计」（本地 SQLite）。选号池与冷却台账需启动服务。
          </p>
        ) : runtimeStatusError ? (
          <div className="cblas-error">{runtimeStatusError}</div>
        ) : runtimeView === "pool" ? (
          <div className="cblas-pool">
            <div style={{ fontSize: 12, opacity: 0.75, marginBottom: 8 }}>
              选号池：三因子权重 · costTier 免费优先 · 在途租约 · WAF/降级冷却
              {runtimeStatus?.pool && (runtimeStatus.pool as { wafIpActive?: boolean }).wafIpActive
                ? " · **WAF IP 拦截中（轮转 fail-fast）**"
                : ""}
            </div>
            {(() => {
              const pool = runtimeStatus?.pool as
                | {
                    accounts?: Array<{
                      id: string;
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
                        { costPer1k?: number; tier?: number; samples?: number }
                      >;
                    }>;
                    costExploreEvents?: number;
                  }
                | undefined;
              const accounts = pool?.accounts ?? [];
              if (!state?.running) {
                return <p className="cblas-empty">启动服务后可查看实时池状态。</p>;
              }
              if (accounts.length === 0) {
                return <p className="cblas-empty">池中暂无账号运行态（成功请求后会开始学习成本）。</p>;
              }
              return (
                <>
                  <div style={{ fontSize: 12, opacity: 0.75, marginBottom: 8 }}>
                    cost explore 事件：{pool?.costExploreEvents ?? 0}
                  </div>
                  <table className="cblas-table">
                    <thead>
                      <tr>
                        <th>账号</th>
                        <th>域</th>
                        <th>积分</th>
                        <th>权重</th>
                        <th>在途</th>
                        <th>状态</th>
                        <th>模型成本</th>
                      </tr>
                    </thead>
                    <tbody>
                      {accounts.map((acc) => {
                        const costs = Object.entries(acc.modelCosts ?? {}).map(([m, c]) => {
                          const tier =
                            c.tier === 0 ? "免费" : c.tier === 2 ? "收费" : "未知";
                          return `${m}:${tier}(${formatCredit(c.costPer1k)})`;
                        });
                        const status = acc.disabled
                          ? `禁用 ${acc.disableReason || ""}`
                          : acc.wafUntil
                            ? `WAF冷却至 ${formatTs(acc.wafUntil)}`
                            : acc.degradeUntil
                              ? `降权至 ${formatTs(acc.degradeUntil)}`
                              : "可用";
                        return (
                          <tr key={acc.id}>
                            <td>
                        {acc.label || acc.id}
                        {acc.alwaysFreeModels?.length ? (
                          <div style={{ fontSize: 11 }}>
                            <span className="cblas-model-tag is-free">
                              全天免费 {acc.alwaysFreeModels.join("/")}
                            </span>
                          </div>
                        ) : null}
                        {acc.nightOnlyFreeModels?.length ? (
                          <div style={{ fontSize: 11 }}>
                            <span className="cblas-model-tag is-paid">
                              仅夜间 {acc.nightOnlyFreeModels.join("/")}
                            </span>
                          </div>
                        ) : null}
                      </td>
                            <td>{acc.realm || "—"}</td>
                            <td>
                              {acc.creditsKnown ? acc.credits : "—"}
                              {acc.creditsExpiring ? ` / 快过期 ${acc.creditsExpiring}` : ""}
                            </td>
                            <td>{formatCredit(acc.weight)}</td>
                            <td>
                              {acc.inFlight ?? 0}/{acc.maxInFlight ?? 2}
                            </td>
                            <td>{status}</td>
                            <td className="cblas-mono" style={{ fontSize: 11 }}>
                              {costs.length ? costs.join(" · ") : "待实测"}
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </>
              );
            })()}
          </div>
        ) : runtimeView === "ledger" ? (
          <div>
            <div className="cblas-kpi-foot" style={{ marginBottom: 8 }}>
              冷却中的账号/模型与截止时间（到点自动恢复；冷却中会换号，全冷却则拦截不打上游）
            </div>
            {coolingRows.length === 0 && (runtimeStatus?.rateLimitedModels ?? []).length === 0 ? (
              <p className="cblas-empty">当前没有冷却中的账号/模型。</p>
            ) : (
              <table className="cblas-table">
                <thead>
                  <tr>
                    <th>账号</th>
                    <th>模型</th>
                    <th>类型</th>
                    <th>冷却至</th>
                    <th>原因</th>
                  </tr>
                </thead>
                <tbody>
                  {coolingRows.map((item, index) => (
                    <tr key={`${item.accountId}-${item.model}-${index}`}>
                      <td>{item.accountLabel || item.accountId || "—"}</td>
                      <td className="cblas-mono">{item.model || "—"}</td>
                      <td>{item.kind || "—"}</td>
                      <td>{formatTs(item.until)}</td>
                      <td>{item.reason || "—"}</td>
                    </tr>
                  ))}
                  {(runtimeStatus?.rateLimitedModels ?? []).map((item, index) => (
                    <tr key={`rl-${item.accountId || item.id}-${item.model}-${index}`}>
                      <td>{item.accountLabel || item.accountId || item.id || "—"}</td>
                      <td className="cblas-mono">{item.model}</td>
                      <td>model</td>
                      <td>{formatTs(item.until)}</td>
                      <td>{item.reason || "6004"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            <div className="cblas-row" style={{ marginTop: 12 }}>
              <button
                type="button"
                className="cblas-button"
                disabled={!state?.running}
                onClick={() => {
                  void run(async () => {
                    await service.clearCodebuddyStickySessions();
                    await refreshRuntimeStatus();
                    return await service.getCodebuddyLocalAccessState();
                  });
                }}
              >
                清除会话粘性
              </button>
              <span className="cblas-kpi-foot">
                当前粘性 {runtimeStatus?.sessionAffinityCount ?? 0} 条 · 换号：冷却中自动解绑并选下一号
              </span>
            </div>
          </div>
        ) : runtimeView === "usage" ? (
          <div>
            <div style={{ fontSize: 12, opacity: 0.75, marginBottom: 8 }}>
              会话粘性绑定中：{runtimeStatus?.sessionAffinityCount ?? 0} 个 ·
              同会话钉在同一账号，减少 prompt cache 打穿
            </div>
            {modelUsage.length === 0 ? (
              <p className="cblas-empty">暂无成功请求用量。发起对话后会按模型聚合。</p>
            ) : (
              <div className="cblas-requests-wrap">
                <table className="cblas-table cblas-usage-table">
                  <thead>
                    <tr>
                      <th>模型</th>
                      <th>请求</th>
                      <th>输入</th>
                      <th>缓存读</th>
                      <th>缓存写</th>
                      <th>总输入</th>
                      <th>输出</th>
                      <th>命中率</th>
                      <th>合计</th>
                    </tr>
                  </thead>
                  <tbody>
                    {modelUsage.map((row) => (
                      <tr key={row.model}>
                        <td>
                          <div className="cblas-mono" title={row.model}>
                            {row.model}
                          </div>
                          <div className="cblas-muted" style={{ fontSize: 11 }}>
                            最近 {row.lastAt ? formatShortTs(row.lastAt) : "—"}
                          </div>
                        </td>
                        <td>{row.requests}</td>
                        <td>{formatTokenCount(row.inputTokens)}</td>
                        <td>{formatTokenCount(row.cacheRead)}</td>
                        <td>{formatTokenCount(row.cacheWrite)}</td>
                        <td>{formatTokenCount(row.totalInput)}</td>
                        <td>{formatTokenCount(row.outputTokens)}</td>
                        <td>
                          {row.totalInput > 0
                            ? `${row.cacheHitPct >= 99.5 ? "99" : Math.round(row.cacheHitPct)}%`
                            : "—"}
                        </td>
                        <td>
                          <strong>{formatTokenCount(row.totalTokens)}</strong>
                          {row.credit ? (
                            <div className="cblas-muted" style={{ fontSize: 11 }}>
                              积分 {formatCredit(row.credit)}
                            </div>
                          ) : null}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        ) : (
          <div>
            <div
              style={{
                display: "flex",
                flexWrap: "wrap",
                gap: 8,
                alignItems: "center",
                marginBottom: 8,
                fontSize: 12,
                opacity: 0.8,
              }}
            >
              <span>
                共 {requestTotal} 条（SQLite 全量） · 每页
              </span>
              <select
                className="cblas-select"
                style={{ width: "auto", minWidth: 72 }}
                value={String(requestPageSize)}
                disabled={requestsLoading}
                onChange={(e) => {
                  const size = Number(e.target.value) || 50;
                  void loadRequestPage(0, size);
                }}
              >
                {[20, 50, 100, 200, 500].map((n) => (
                  <option key={n} value={String(n)}>
                    {n}
                  </option>
                ))}
              </select>
              <span>
                第 {requestTotal === 0 ? 0 : requestPage + 1} /{" "}
                {Math.max(1, Math.ceil(requestTotal / requestPageSize))} 页
              </span>
              <button
                type="button"
                className="cblas-button"
                disabled={requestsLoading || requestPage <= 0}
                onClick={() => void loadRequestPage(requestPage - 1, requestPageSize)}
              >
                上一页
              </button>
              <button
                type="button"
                className="cblas-button"
                disabled={
                  requestsLoading ||
                  (requestPage + 1) * requestPageSize >= requestTotal
                }
                onClick={() => void loadRequestPage(requestPage + 1, requestPageSize)}
              >
                下一页
              </button>
            </div>
            {requestsLoading ? (
              <p className="cblas-empty">加载中…</p>
            ) : requestRecords.length === 0 ? (
              <p className="cblas-empty">暂无请求流水。发起一次对话后会写入本地台账。</p>
            ) : (
              <div className="cblas-requests-wrap">
              <table className="cblas-table cblas-requests-table">
                <thead>
                  <tr>
                    <th>时间</th>
                    <th>账号</th>
                    <th>模型</th>
                    <th>结果</th>
                    <th>首包 / 耗时</th>
                    <th>输入</th>
                    <th>缓存读</th>
                    <th>缓存写</th>
                    <th>总输入</th>
                    <th>输出</th>
                    <th>命中率</th>
                    <th>合计</th>
                    <th>积分</th>
                    <th>详情</th>
                  </tr>
                </thead>
                <tbody>
                  {requestRecords
                    .filter((rec: CodebuddyRequestRecord) => {
                      if (logFilterAccount && !(rec.accountId || rec.accountLabel || "").toLowerCase().includes(logFilterAccount.toLowerCase())) {
                        return false;
                      }
                      if (logFilterModel && !(rec.model || "").toLowerCase().includes(logFilterModel.toLowerCase())) {
                        return false;
                      }
                      if (logFilterOutcome && rec.outcome !== logFilterOutcome) {
                        return false;
                      }
                      return true;
                    })
                    .map((rec: CodebuddyRequestRecord) => {
                    const expanded = expandedRequestId === rec.id;
                    const prompt = rec.promptTokens ?? 0;
                    const cacheRead = rec.cachedTokens ?? 0;
                    const cacheWrite = rec.cacheWriteTokens ?? 0;
                    const output = rec.completionTokens ?? 0;
                    // DeepSeek/CodeBuddy: prompt_tokens usually INCLUDES cache hits.
                    const inclusive = cacheRead > 0 && cacheRead <= prompt;
                    const totalInput = inclusive ? prompt : prompt + cacheRead;
                    const inputNew = inclusive ? Math.max(0, prompt - cacheRead) : prompt;
                    const totalTok =
                      rec.totalTokens != null && rec.totalTokens > 0
                        ? rec.totalTokens
                        : totalInput + output;
                    const hit = totalInput > 0 ? (cacheRead / totalInput) * 100 : null;
                    return (
                      <Fragment key={rec.id || rec.timestamp}>
                        <tr>
                          <td className="cblas-ts-cell" title={rec.timestamp}>
                            {formatShortTs(rec.timestamp)}
                          </td>
                          <td title={rec.accountLabel || rec.accountId || undefined}>
                            {rec.accountLabel || rec.accountId || "—"}
                          </td>
                          <td className="cblas-model-cell" title={rec.model}>
                            <span className="cblas-mono">{rec.model}</span>
                          </td>
                          <td className="cblas-outcome-cell">
                            <span
                              className={`cblas-outcome-pill ${outcomeClass(rec.outcome)}`}
                              title={
                                rec.reasonCode
                                  ? `${outcomeLabel(rec.outcome)} (${rec.reasonCode})`
                                  : outcomeLabel(rec.outcome)
                              }
                            >
                              {outcomeLabel(rec.outcome)}
                            </span>
                          </td>
                          <td className="cblas-latency-cell">
                            <div>{rec.firstTokenMs != null ? `${rec.firstTokenMs}ms` : "—"}</div>
                            <div className="cblas-muted" style={{ fontSize: 11 }}>
                              {rec.totalMs != null || rec.latencyMs != null
                                ? `${rec.totalMs ?? rec.latencyMs}ms`
                                : "—"}
                            </div>
                          </td>
                          <td>{formatTokenCount(inputNew)}</td>
                          <td>{formatTokenCount(cacheRead)}</td>
                          <td>{formatTokenCount(cacheWrite)}</td>
                          <td>{formatTokenCount(totalInput)}</td>
                          <td>{formatTokenCount(output)}</td>
                          <td>{hit != null ? `${Math.round(hit)}%` : "—"}</td>
                          <td>
                            <strong>{formatTokenCount(totalTok)}</strong>
                          </td>
                          <td>
                            {rec.hasCredit || rec.credit != null ? (
                              <span className="cblas-credit-pill" title="本次消耗积分">
                                {formatCredit(rec.credit)}
                              </span>
                            ) : (
                              <span className="cblas-muted">—</span>
                            )}
                          </td>
                          <td>
                            <button
                              type="button"
                              className="cblas-button"
                              onClick={() => setExpandedRequestId(expanded ? null : rec.id)}
                            >
                              {expanded ? "收起" : "详情"}
                            </button>
                          </td>
                        </tr>
                        {expanded ? (
                          <tr className="cblas-request-detail-row">
                            <td colSpan={14}>
                              <div className="cblas-request-detail">
                                <div>
                                  <strong>请求参数</strong>
                                  <ul>
                                    <li>流式：{rec.clientStream ? "是" : "否"}（上游恒 stream）</li>
                                    <li>
                                      max_tokens：{rec.maxTokens ?? "默认"} · temp：
                                      {rec.temperature ?? "默认"} · top_p：{rec.topP ?? "默认"}
                                    </li>
                                    <li>
                                      消息数 {rec.messageCount ?? 0} · system 长度{" "}
                                      {rec.systemChars ?? 0} 字符 · tools {rec.toolCount ?? 0}
                                      {rec.toolChoice ? ` · tool_choice=${rec.toolChoice}` : ""}
                                    </li>
                                    <li>
                                      提示词：{rec.promptMode || "passthrough"}
                                      {rec.degradedPrompt ? "（已降级中性）" : ""}
                                      {rec.includeReasoning ? " · 返回思考" : ""}
                                    </li>
                                  </ul>
                                </div>
                                <div>
                                  <strong>响应 / 治理</strong>
                                  <ul>
                                    <li>
                                      HTTP {rec.httpStatus || "—"} · 重试序号 {rec.attempt || 1}
                                      · finish {rec.finishReason || "—"}
                                    </li>
                                    <li>
                                      首包 {rec.firstTokenMs != null ? `${rec.firstTokenMs}ms` : "—"} ·
                                      总耗时{" "}
                                      {rec.totalMs != null
                                        ? `${rec.totalMs}ms`
                                        : rec.latencyMs != null
                                          ? `${rec.latencyMs}ms`
                                          : "—"}
                                    </li>
                                    <li>会话：{rec.conversationRequestId || "—"}</li>
                                    <li>
                                      token：新输入 {inputNew} / 缓存读 {cacheRead} / 缓存写{" "}
                                      {cacheWrite} / 总输入 {totalInput} / 输出 {output} / 合计{" "}
                                      {totalTok}
                                      {rec.reasoningTokens ? ` / 思考 ${rec.reasoningTokens}` : ""}
                                    </li>
                                    <li>
                                      积分：
                                      {rec.hasCredit || rec.credit != null
                                        ? ` ${formatCredit(rec.credit)}`
                                        : " 上游未返回"}
                                    </li>
                                    {rec.resetAt ? <li>限流重置：{formatTs(rec.resetAt)}</li> : null}
                                  </ul>
                                </div>
                                {rec.message ? (
                                  <div style={{ gridColumn: "1 / -1" }}>
                                    <strong>错误 / 摘要</strong>
                                    <pre className="cblas-request-detail-pre">{rec.message}</pre>
                                  </div>
                                ) : null}
                              </div>
                            </td>
                          </tr>
                        ) : null}
                      </Fragment>
                    );
                  })}
                </tbody>
              </table>
              </div>
            )}
          </div>
        )}
      </section>

      <section className="cblas-card" hidden={pageTab !== "overview" && pageTab !== "keys"}>
        <h3>{pageTab === "keys" ? "连接信息" : "服务控制"}</h3>
        <div className="cblas-row">
          <button
            type="button"
            className="cblas-button cblas-button--primary"
            onClick={handleToggleService}
            disabled={busy || !collection || (!state?.running && selectedRefs.length === 0)}
          >
            {state?.running ? <Square size={14} /> : <Play size={14} />}
            <span style={{ marginLeft: 6 }}>{state?.running ? "停止服务" : "启动服务"}</span>
          </button>
          <button
            type="button"
            className="cblas-button"
            onClick={() => void run(() => service.restartCodebuddyLocalAccess())}
            disabled={busy || !state?.running}
          >
            <RefreshCw size={14} />
            <span style={{ marginLeft: 6 }}>重启</span>
          </button>
          <button
            type="button"
            className="cblas-button"
            onClick={handleTest}
            disabled={busy || !state?.running}
          >
            <Zap size={14} />
            <span style={{ marginLeft: 6 }}>测试连接</span>
          </button>
          <button
            type="button"
            className="cblas-button cblas-button--primary"
            onClick={openProbe}
            disabled={!state?.running}
          >
            <Wand2 size={14} />
            <span style={{ marginLeft: 6 }}>测试模型</span>
          </button>
        </div>

        {state?.baseUrl ? (
          <div className="cblas-row">
            <span className="cblas-label">Base URL</span>
            <span className="cblas-mono">{state.baseUrl}</span>
            <button type="button" className="cblas-button" onClick={() => void copy("base", state.baseUrl ?? "")}>
              {copied === "base" ? "已复制" : "复制"}
            </button>
          </div>
        ) : null}

        {collection?.apiKey ? (
          <div className="cblas-row">
            <span className="cblas-label">API Key</span>
            <span className="cblas-mono">{collection.apiKey}</span>
            <button
              type="button"
              className="cblas-button"
              onClick={() => void copy("key", collection.apiKey)}
            >
              {copied === "key" ? "已复制" : "复制"}
            </button>
            <button
              type="button"
              className="cblas-button"
              onClick={() =>
                void run(async () => {
                  const next = await service.rotateCodebuddyLocalAccessApiKey();
                  return next;
                })
              }
              disabled={busy}
            >
              轮换密钥
            </button>
          </div>
        ) : null}

        <div className="cblas-row" style={{ alignItems: "flex-start" }}>
          <span className="cblas-label">附加密钥</span>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div style={{ fontSize: 12, opacity: 0.75, marginBottom: 6 }}>
              可给不同客户端发独立密钥；某一 Key 频繁中断只会冷却该 Key，不会打死账号池。
              下方可按 Key 查看调用量与流水。
            </div>
            <table className="cblas-table" style={{ marginBottom: 10 }}>
              <thead>
                <tr>
                  <th>Key</th>
                  <th>调用</th>
                  <th>成功</th>
                  <th>失败</th>
                  <th>Tokens</th>
                  <th>Credit</th>
                  <th>流水</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td>Primary</td>
                  <td>{apiKeyStats["(unknown)"]?.requests ?? apiKeyStats["Primary"]?.requests ?? "—"}</td>
                  <td>{apiKeyStats["(unknown)"]?.ok ?? apiKeyStats["Primary"]?.ok ?? "—"}</td>
                  <td>{apiKeyStats["(unknown)"]?.fail ?? apiKeyStats["Primary"]?.fail ?? "—"}</td>
                  <td>{formatTokenCount(apiKeyStats["(unknown)"]?.tokens ?? apiKeyStats["Primary"]?.tokens)}</td>
                  <td>{formatCredit(apiKeyStats["(unknown)"]?.credit ?? apiKeyStats["Primary"]?.credit)}</td>
                  <td>
                    <button
                      type="button"
                      className="cblas-button"
                      onClick={() => {
                        setApiKeyFilter("");
                        setRuntimeView("requests");
                        void loadRequestPage(0, requestPageSize, "");
                      }}
                    >
                      查看
                    </button>
                  </td>
                </tr>
                {(collection?.clientKeys ?? []).map((item) => {
                  const st =
                    apiKeyStats[item.id] ||
                    apiKeyStats[item.label] ||
                    Object.entries(apiKeyStats).find(
                      ([k]) => k.includes(item.id) || k.includes(item.label),
                    )?.[1] ||
                    {};
                  return (
                    <tr key={item.id}>
                      <td>
                        {item.label || item.id}
                        {!item.enabled ? "（停用）" : ""}
                      </td>
                      <td>{st.requests ?? "—"}</td>
                      <td>{st.ok ?? "—"}</td>
                      <td>{st.fail ?? "—"}</td>
                      <td>{formatTokenCount(st.tokens)}</td>
                      <td>{formatCredit(st.credit)}</td>
                      <td>
                        <button
                          type="button"
                          className="cblas-button"
                          onClick={() => {
                            setApiKeyFilter(item.id || item.label);
                            setRuntimeView("requests");
                            void loadRequestPage(0, requestPageSize, item.id || item.label);
                          }}
                        >
                          查看
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            {(collection?.clientKeys ?? []).map((item, index) => (
              <div className="cblas-row" key={item.id || index} style={{ marginBottom: 6 }}>
                <input
                  className="cblas-input"
                  style={{ width: 120 }}
                  value={item.label}
                  placeholder="命名"
                  onChange={(e) => {
                    const next = [...(collection?.clientKeys ?? [])];
                    next[index] = { ...item, label: e.target.value };
                    // 改名不重启服务，只热更新运行文件 + 本地配置
                    void run(() => service.saveCodebuddyClientKeys(next));
                  }}
                  disabled={busy}
                />
                <span className="cblas-mono" style={{ flex: 1, overflow: "hidden", textOverflow: "ellipsis" }}>
                  {item.key}
                </span>
                <button
                  type="button"
                  className="cblas-button"
                  onClick={() => void copy(`ck-${item.id}`, item.key)}
                >
                  {copied === `ck-${item.id}` ? "已复制" : "复制"}
                </button>
                <button
                  type="button"
                  className="cblas-button"
                  onClick={() => {
                    const next = (collection?.clientKeys ?? []).filter((_, i) => i !== index);
                    void run(() => service.saveCodebuddyClientKeys(next));
                  }}
                  disabled={busy}
                >
                  删除
                </button>
              </div>
            ))}
            <button
              type="button"
              className="cblas-button"
              disabled={busy || !collection}
              onClick={() => {
                const key = `cbk-${Math.random().toString(36).slice(2, 10)}${Math.random()
                  .toString(36)
                  .slice(2, 10)}`;
                const next = [
                  ...(collection?.clientKeys ?? []),
                  {
                    id: `ck_${Date.now()}`,
                    label: `Key ${(collection?.clientKeys?.length ?? 0) + 1}`,
                    key,
                    enabled: true,
                  },
                ];
                void run(() => service.saveCodebuddyClientKeys(next));
              }}
            >
              添加密钥
            </button>
            <div style={{ fontSize: 12, opacity: 0.65, marginTop: 6 }}>
              改名 / 增删附加密钥不会重启服务；若网关层鉴权未立刻生效，可手动点一次「重启服务」。
            </div>
          </div>
        </div>

        {state?.lanBaseUrl ? (
          <div className="cblas-row">
            <span className="cblas-label">局域网地址</span>
            <span className="cblas-mono">{state.lanBaseUrl}</span>
          </div>
        ) : null}

        {testResult ? (
          <div className={testResult.ok ? "cblas-ok" : "cblas-error"}>
            {testResult.message}
            {testResult.latencyMs != null ? ` · ${testResult.latencyMs}ms` : ""}
          </div>
        ) : null}
      </section>

      <section className="cblas-card" hidden={pageTab !== "accounts" && pageTab !== "overview"}>
        <h3>配置</h3>
        <div className="cblas-row">
          <span className="cblas-label">监听端口</span>
          <input
            className="cblas-input"
            value={portDraft}
            onChange={(event) => setPortDraft(event.target.value)}
            onBlur={handlePortCommit}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                handlePortCommit();
              }
            }}
            disabled={busy}
          />
          <span className="cblas-subtitle" style={{ margin: 0 }}>
            修改端口后服务会自动重启
          </span>
        </div>
        <div className="cblas-row">
          <span className="cblas-label">绑定范围</span>
          <select
            className="cblas-select"
            value={collection?.accessScope ?? "localhost"}
            onChange={(event) => handleScopeChange(event.target.value as CodebuddyLocalAccessScope)}
            disabled={busy}
          >
            <option value="localhost">仅本机 (127.0.0.1)</option>
            <option value="lan">局域网 (0.0.0.0)</option>
          </select>
        </div>
        <div className="cblas-row">
          <span className="cblas-label">账号轮换</span>
          <select
            className="cblas-select"
            value={collection?.routingStrategy ?? "roundRobin"}
            onChange={(event) =>
              handleRoutingChange(event.target.value as "roundRobin" | "random")
            }
            disabled={busy}
          >
            <option value="roundRobin">轮询（默认）</option>
            <option value="random">随机</option>
          </select>
        </div>
        <div className="cblas-row">
          <span className="cblas-label">思考过程</span>
          <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13 }}>
            <input
              type="checkbox"
              checked={collection?.includeReasoning ?? false}
              onChange={(event) => handleReasoningChange(event.target.checked)}
              disabled={busy}
            />
            在响应中返回 reasoning_content（部分客户端不支持）
          </label>
        </div>
      </section>

      <section className="cblas-card" hidden={pageTab !== "accounts"}>
        <div className="cblas-card-head">
          <h3>
            账号池（{selectedRefs.length}/{flatAccounts.length} 已启用）
          </h3>
          <div className="cblas-row" style={{ marginBottom: 0 }}>
            <button
              type="button"
              className="cblas-button"
              onClick={selectAllAccounts}
              disabled={busy || !collection || flatAccounts.length === 0}
              title="启用全部账号"
            >
              全选
            </button>
            <button
              type="button"
              className="cblas-button"
              onClick={clearSelectedAccounts}
              disabled={busy || !collection || selectedRefs.length === 0}
              title="取消全部启用"
            >
              全不选
            </button>
            <button
              type="button"
              className="cblas-button"
              onClick={invertSelectedAccounts}
              disabled={busy || !collection || flatAccounts.length === 0}
              title="反选启用"
            >
              反选
            </button>
            <span style={{ fontSize: 12, opacity: 0.7 }}>
              单击点选 · Shift+单击连选
            </span>
          </div>
        </div>
        {groups.length === 0 ? (
          <p className="cblas-empty">
            未检测到 CodeBuddy / WorkBuddy 账号。请先在对应的管理页面登录账号。
          </p>
        ) : (
          <div className="cblas-account-table-wrap">
            <table className="cblas-table cblas-account-table">
              <thead>
                <tr>
                  <th style={{ width: 36 }}>启用</th>
                  <th>账号</th>
                  <th style={{ width: 140 }}>积分</th>
                  <th style={{ width: 150 }}>令牌有效期</th>
                  <th>模型限流冷却</th>
                </tr>
              </thead>
              {groups.map((group) => {
                const groupSelected = group.accounts.filter((option) =>
                  isSelected(option.platform, option.accountId),
                ).length;
                const indexOfAccount = (option: CodebuddyLocalAccessAccountOption) =>
                  flatAccounts.findIndex(
                    (item) =>
                      item.platform === option.platform && item.accountId === option.accountId,
                  );
                return (
                  <tbody key={group.platform}>
                    <tr className="cblas-account-group-row">
                      <td colSpan={5}>
                        <div className="cblas-account-group-title">
                          <span>
                            {group.name}（{groupSelected}/{group.accounts.length}）
                          </span>
                          <button
                            type="button"
                            className="cblas-button"
                            style={{ marginLeft: 8 }}
                            disabled={busy || !collection || group.accounts.length === 0}
                            onClick={() => selectGroupAccounts(group.accounts)}
                            title={`启用该分组全部 ${group.accounts.length} 个账号`}
                          >
                            本组全选
                          </button>
                        </div>
                      </td>
                    </tr>
                    {group.accounts.map((option) => {
                      const index = indexOfAccount(option);
                      const coolings = cooldownsByAccount.get(option.accountId) ?? [];
                      return (
                        <tr
                          key={`${option.platform}-${option.accountId}`}
                          className={coolings.length > 0 ? "cblas-account-row--cooling" : ""}
                        >
                          <td>
                            <input
                              type="checkbox"
                              checked={isSelected(option.platform, option.accountId)}
                              onChange={(e) =>
                                toggleAccount(option, Math.max(index, 0), {
                                  shiftKey: (e.nativeEvent as MouseEvent).shiftKey,
                                })
                              }
                              disabled={busy}
                              title="点击切换启用；按住 Shift 连选区间"
                            />
                          </td>
                          <td>
                            <div className="cblas-account-name">
                              {option.label || option.accountId}
                            </div>
                          </td>
                          <td>
                            {option.creditsRemain != null ? (
                              <span
                                className="cblas-credits"
                                title="套餐积分剩余 / 总量"
                              >
                                {option.creditsRemain}
                                {option.creditsSize != null ? (
                                  <span className="cblas-credits-size"> / {option.creditsSize}</span>
                                ) : null}
                              </span>
                            ) : (
                              <span className="cblas-muted">—</span>
                            )}
                          </td>
                          <td>
                            {option.tokenAvailable ? (
                              <span className="cblas-token">{formatExpiry(option.expiresAtMs)}</span>
                            ) : (
                              <span className="cblas-token-missing">缺少令牌</span>
                            )}
                          </td>
                          <td>
                            {coolings.length === 0 ? (
                              <span className="cblas-muted">—</span>
                            ) : (
                              <div className="cblas-cooling-list">
                                {coolings.map((cool) => (
                                  <div
                                    key={cool.model}
                                    className="cblas-cooling-chip"
                                    title={cool.reason || "6004 model rate limit"}
                                  >
                                    <span className="cblas-mono">{cool.model}</span>
                                    <span>至 {formatTs(cool.until || cool.resetAt)}</span>
                                  </div>
                                ))}
                              </div>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                );
              })}
            </table>
          </div>
        )}
      </section>

      <section className="cblas-card" hidden={pageTab !== "models"}>
        <h3>
          模型目录
          <span style={{ fontWeight: 400, fontSize: 12, marginLeft: 8, opacity: 0.7 }}>
            参数 · 积分倍率 · 实测是否免费 · 调用消耗 · 可禁用
          </span>
        </h3>
        <div className="cblas-row">
          <button
            type="button"
            className="cblas-button"
            onClick={() => void handleFetchModels()}
            disabled={fetchingModels || !collection || selectedRefs.length === 0}
          >
            <Download size={14} />
            <span style={{ marginLeft: 6 }}>{fetchingModels ? "拉取中…" : "拉取企业模型"}</span>
          </button>
          {fetchedModels.length > 0 ? (
            <button
              type="button"
              className="cblas-button cblas-button--primary"
              onClick={handleApplyFetchedModels}
              disabled={busy}
            >
              应用为可用模型（{usableModelIds.length}）
            </button>
          ) : null}
          <span className="cblas-subtitle" style={{ margin: 0 }}>
            拉取后写入 catalog（上下文/efforts），sidecar 合并 models.dev 与实测 credit
          </span>
        </div>
        {runtimeStatus?.costExplore ? (
          <div style={{ fontSize: 12, opacity: 0.75, margin: "6px 0" }}>
            cost explore：{runtimeStatus.costExplore.enabled ? "开" : "关"} · 窗口{" "}
            {runtimeStatus.costExplore.interval || "30m"} ·{" "}
            {runtimeStatus.costExplore.note || "免费优先探索未知号"}
          </div>
        ) : null}
        <div className="cblas-row" style={{ margin: "8px 0" }}>
          <span className="cblas-label">夜间免费</span>
          <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12 }}>
            <input
              type="checkbox"
              checked={!!collection?.nightFreeEnabled}
              onChange={(e) => {
                void run(async () => {
                  await service.setCodebuddyNightFree(
                    e.target.checked,
                    collection?.nightFreeModels ?? ["hy3"],
                  );
                  return await service.restartCodebuddyLocalAccess();
                });
              }}
              disabled={busy}
            />
            启用 23:00–08:00 CST 免费窗口（窗外拒绝，防计费）
          </label>
          <input
            className="cblas-input"
            style={{ flex: 1, minWidth: 180 }}
            value={(collection?.nightFreeModels ?? []).join(",")}
            placeholder="hy3,deepseek-v4.1-flash"
            onChange={(e) => {
              const list = e.target.value
                .split(",")
                .map((s) => s.trim())
                .filter(Boolean);
              void run(() =>
                service.setCodebuddyNightFree(!!collection?.nightFreeEnabled, list).then(
                  service.restartCodebuddyLocalAccess,
                ),
              );
            }}
            disabled={busy}
          />
          {runtimeStatus?.nightFree ? (
            <span style={{ fontSize: 12, opacity: 0.8 }}>
              {runtimeStatus.nightFree.window} ·{" "}
              {runtimeStatus.nightFree.inWindow ? "窗口内" : "窗外（禁用列表模型）"}
            </span>
          ) : null}
        </div>
        {modelsMessage ? <div className="cblas-note">{modelsMessage}</div> : null}
        <div className="cblas-model-table">
          {(() => {
            const catalog = runtimeStatus?.catalog ?? [];
            const disabledSet = new Set(
              (collection?.disabledModels ?? []).map((x) => x.toLowerCase()),
            );
            const usageMap = new Map(
              (modelUsage ?? []).map((row) => [row.model.toLowerCase(), row]),
            );
            const rows: CodebuddyModelCatalogRow[] =
              catalog.length > 0
                ? catalog
                : (fetchedModels.length > 0
                    ? fetchedModels
                    : (state?.modelIds ?? []).map((id) => ({ id, name: id, cli: true }))
                  ).map((m) => ({
                    id: m.id,
                    name: (m as { name?: string }).name,
                    contextLength: (m as { contextLength?: number }).contextLength,
                    maxOutputTokens: (m as { maxOutputTokens?: number }).maxOutputTokens,
                    efforts: (m as { efforts?: string[] }).efforts ?? [],
                    supportsImages: (m as { supportsImages?: boolean }).supportsImages,
                    supportsReasoning: (m as { supportsReasoning?: boolean }).supportsReasoning,
                    cli: (m as { cli?: boolean }).cli,
                    description: (m as { description?: string }).description,
                    credits: (m as { credits?: string }).credits,
                    creditsRate: (m as { creditsRate?: number }).creditsRate,
                    tags: (m as { tags?: string[] }).tags ?? [],
                    vendor: (m as { vendor?: string }).vendor,
                    isDefault: (m as { isDefault?: boolean }).isDefault,
                    onlyReasoning: (m as { onlyReasoning?: boolean }).onlyReasoning,
                    maxAllowedSize: (m as { maxAllowedSize?: number }).maxAllowedSize,
                    disabled: disabledSet.has(m.id.toLowerCase()),
                  }));
            return rows.map((model) => {
              const usage = usageMap.get(model.id.toLowerCase());
              const free =
                model.freeObserved === true ||
                (model.freeObserved == null &&
                  typeof model.creditsRate === "number" &&
                  model.creditsRate === 0);
              const isDisabled = disabledSet.has(model.id.toLowerCase()) || !!model.disabled;
              return (
                <div className={`cblas-model-row ${isDisabled ? "is-disabled" : ""}`} key={model.id}>
                  <div className="cblas-model-main">
                    <span className="cblas-model-id">{model.id}</span>
                    {model.name && model.name !== model.id ? (
                      <span className="cblas-model-name">{model.name}</span>
                    ) : null}
                    {model.cli !== false ? <span className="cblas-model-tag">CLI</span> : null}
                    {model.isDefault ? <span className="cblas-model-tag">默认</span> : null}
                    {model.supportsImages ? <span className="cblas-model-tag">多模态</span> : null}
                    {model.supportsReasoning || model.onlyReasoning ? (
                      <span className="cblas-model-tag">推理</span>
                    ) : null}
                    {(model.tags ?? []).map((tag) => (
                      <span
                        key={tag}
                        className={`cblas-model-tag ${
                          tag.includes("限时免费") || /free/i.test(tag) ? "is-free" : ""
                        }`}
                      >
                        {tag}
                      </span>
                    ))}
                    <span className={`cblas-model-tag ${free ? "is-free" : "is-paid"}`}>
                      {model.credits
                        ? `credits ${model.credits}`
                        : model.creditsRate != null
                          ? `credits x${model.creditsRate}`
                          : free
                            ? "免费/限免"
                            : "计费未知"}
                    </span>
                    {isDisabled ? <span className="cblas-model-tag is-off">已禁用</span> : null}
                  </div>
                  <div className="cblas-model-meta">
                    上下文 {model.contextLength ? `${Math.round((model.contextLength || 0) / 1000)}K` : "—"}
                    {" · "}输出 {model.maxOutputTokens ? `${Math.round((model.maxOutputTokens || 0) / 1000)}K` : "—"}
                    {model.maxAllowedSize ? ` · 允许输入 ${Math.round(model.maxAllowedSize / 1000)}K` : ""}
                    {model.efforts?.length ? ` · 档位 ${model.efforts.join("/")}` : ""}
                    {model.vendor ? ` · vendor ${model.vendor}` : ""}
                    {model.source ? ` · 来源 ${model.source}` : ""}
                  </div>
                  <div className="cblas-model-meta">
                    原始字段：credits=
                    <span className="cblas-mono">{model.credits || "—"}</span>
                    {model.creditsRate != null ? (
                      <>
                        {" "}
                        rate=
                        <span className="cblas-mono">{model.creditsRate}</span>
                      </>
                    ) : null}
                    {" · tags="}

                    <span className="cblas-mono">
                      {(model.tags ?? []).length ? (model.tags ?? []).join(" | ") : "—"}
                    </span>
                    {model.vendor ? (
                      <>
                        {" · vendor="}

                        <span className="cblas-mono">{model.vendor}</span>
                      </>
                    ) : null}
                    {model.onlyReasoning ? " · onlyReasoning" : ""}
                  </div>
                  <div className="cblas-model-meta">
                    消耗：请求 {usage?.requests ?? model.requests ?? 0} · tokens{" "}
                    {formatTokenCount(usage?.totalTokens ?? model.totalTokens)} · credit{" "}
                    {formatCredit(usage?.credit ?? model.totalCredit)}
                    {model.costPer1k != null
                      ? ` · 实测 ${model.costTier === 0 ? "免费" : `${formatCredit(model.costPer1k)}/1k`}`
                      : " · 实测待学习"}
                  </div>
                  {model.accountsAlwaysFree?.length || model.accountsNightFree?.length ? (
                    <div className="cblas-model-meta">
                      {model.accountsAlwaysFree?.length ? (
                        <span className="cblas-model-tag is-free" style={{ marginRight: 6 }}>
                          全天免费号：{model.accountsAlwaysFree.join("、")}
                        </span>
                      ) : null}
                      {model.accountsNightFree?.length ? (
                        <span className="cblas-model-tag is-paid">
                          仅夜间免费号：{model.accountsNightFree.join("、")}
                        </span>
                      ) : null}
                      {model.freePolicySummary ? (
                        <div style={{ opacity: 0.8 }}>{model.freePolicySummary}</div>
                      ) : null}
                    </div>
                  ) : null}
                  {model.description ? <div className="cblas-model-desc">{model.description}</div> : null}
                  <div style={{ marginTop: 4, display: "flex", gap: 6, flexWrap: "wrap" }}>
                    <button
                      type="button"
                      className="cblas-button cblas-button--primary"
                      disabled={busy || !collection?.accounts?.length}
                      title="把该模型加入所有已选账号的「全天免费」分组"
                      onClick={() => {
                        void (async () => {
                          setBusy(true);
                          try {
                            for (const acc of collection?.accounts ?? []) {
                              const prev =
                                collection?.accounts?.find(
                                  (a) =>
                                    a.accountId === acc.accountId &&
                                    a.platform === acc.platform,
                                ) ?? acc;
                              const always = new Set(prev.alwaysFreeModels ?? []);
                              always.add(model.id);
                              const night = new Set(prev.nightOnlyFreeModels ?? []);
                              night.delete(model.id);
                              await service.setCodebuddyAccountFreeModels({
                                accountId: acc.accountId,
                                platform: acc.platform,
                                alwaysFreeModels: Array.from(always),
                                nightOnlyFreeModels: Array.from(night),
                              });
                            }
                            const next = await service.restartCodebuddyLocalAccess();
                            setState(next);
                            setPortDraft(String(next.collection.port));
                            setModelsMessage(`${model.id} 已加入全天免费分组（所有已选账号）`);
                          } catch (err) {
                            setError(err instanceof Error ? err.message : String(err));
                          } finally {
                            setBusy(false);
                          }
                        })();
                      }}
                    >
                      加入全天免费
                    </button>
                    <button
                      type="button"
                      className="cblas-button"
                      disabled={busy || !collection?.accounts?.length}
                      title="把该模型加入所有已选账号的「仅夜间免费」分组"
                      onClick={() => {
                        void (async () => {
                          setBusy(true);
                          try {
                            for (const acc of collection?.accounts ?? []) {
                              const prev =
                                collection?.accounts?.find(
                                  (a) =>
                                    a.accountId === acc.accountId &&
                                    a.platform === acc.platform,
                                ) ?? acc;
                              const night = new Set(prev.nightOnlyFreeModels ?? []);
                              night.add(model.id);
                              const always = new Set(prev.alwaysFreeModels ?? []);
                              always.delete(model.id);
                              await service.setCodebuddyAccountFreeModels({
                                accountId: acc.accountId,
                                platform: acc.platform,
                                alwaysFreeModels: Array.from(always),
                                nightOnlyFreeModels: Array.from(night),
                              });
                            }
                            const next = await service.restartCodebuddyLocalAccess();
                            setState(next);
                            setModelsMessage(`${model.id} 已加入仅夜间免费分组（所有已选账号）`);
                          } catch (err) {
                            setError(err instanceof Error ? err.message : String(err));
                          } finally {
                            setBusy(false);
                          }
                        })();
                      }}
                    >
                      加入仅夜间免费
                    </button>
                    <button
                      type="button"
                      className="cblas-button"
                      disabled={busy || !collection?.accounts?.length}
                      title="从所有已选账号的免费分组移除"
                      onClick={() => {
                        void (async () => {
                          setBusy(true);
                          try {
                            for (const acc of collection?.accounts ?? []) {
                              const prev =
                                collection?.accounts?.find(
                                  (a) =>
                                    a.accountId === acc.accountId &&
                                    a.platform === acc.platform,
                                ) ?? acc;
                              const always = (prev.alwaysFreeModels ?? []).filter(
                                (x) => x.toLowerCase() !== model.id.toLowerCase(),
                              );
                              const night = (prev.nightOnlyFreeModels ?? []).filter(
                                (x) => x.toLowerCase() !== model.id.toLowerCase(),
                              );
                              await service.setCodebuddyAccountFreeModels({
                                accountId: acc.accountId,
                                platform: acc.platform,
                                alwaysFreeModels: always,
                                nightOnlyFreeModels: night,
                              });
                            }
                            const next = await service.restartCodebuddyLocalAccess();
                            setState(next);
                            setModelsMessage(`${model.id} 已移出免费分组`);
                          } catch (err) {
                            setError(err instanceof Error ? err.message : String(err));
                          } finally {
                            setBusy(false);
                          }
                        })();
                      }}
                    >
                      移出分组
                    </button>
                    <button
                      type="button"
                      className={`cblas-button ${isDisabled ? "" : "cblas-button"}`}
                      onClick={() => {
                        const next = new Set(disabledSet);
                        if (isDisabled) {
                          next.delete(model.id.toLowerCase());
                        } else {
                          next.add(model.id.toLowerCase());
                        }
                        const list = Array.from(next);
                        void run(async () => {
                          await service.setCodebuddyDisabledModels(list);
                          return await service.restartCodebuddyLocalAccess();
                        });
                      }}
                      disabled={busy}
                    >
                      {isDisabled ? "启用模型" : "禁用模型"}
                    </button>
                  </div>
                </div>
              );
            });
          })()}
        </div>
      </section>

      <section className="cblas-card" hidden={pageTab !== "help"}>
        <h3>接入说明</h3>
        <div className="cblas-usage">
          1. 点击「启动服务」，确认状态为「运行中」。<br />
          2. 在第三方客户端填入上方 Base URL（OpenAI 兼容）与 API Key。<br />
          3. 模型名直接使用上方列表中的 ID，例如 <span className="cblas-mono">glm-5.1</span>。<br />
          4. 服务会强制使用流式上游并为本机非流式请求自动聚合结果；上游不支持
          <span className="cblas-mono"> stream:false </span>，这一步由代理完成。<br />
          5. 令牌失效时 sidecar 会自动续期并回写到账号库，无需重新登录。
        </div>
      </section>

      </div>

      {probeOpen ? (
        <div className="cblas-modal-backdrop" role="dialog" aria-modal="true">
          <div className="cblas-modal">
            <div className="cblas-modal-header">
              <h3>测试模型响应</h3>
              <button
                type="button"
                className="cblas-button cblas-icon-button"
                onClick={() => setProbeOpen(false)}
                aria-label="关闭"
              >
                <X size={14} />
              </button>
            </div>
            <p className="cblas-modal-desc">
              经本地反代发起一次真实对话。走完整链路（本机服务 → sidecar → 上游），
              用于确认「该模型 + 该账号」是否真的能响应。
            </p>

            <div className="cblas-row">
              <span className="cblas-label">模型</span>
              <select
                className="cblas-select"
                value={probeModel}
                onChange={(event) => setProbeModel(event.target.value)}
                disabled={probing}
              >
                {probeModelOptions.length === 0 ? <option value="">请先拉取模型</option> : null}
                {probeModelOptions.map((option) => (
                  <option value={option.id} key={option.id}>
                    {option.label === option.id ? option.id : `${option.label}（${option.id}）`}
                  </option>
                ))}
              </select>
              {probeModelOptions.length === 0 ? (
                <button
                  type="button"
                  className="cblas-button"
                  onClick={() => void handleFetchModels()}
                  disabled={fetchingModels}
                >
                  {fetchingModels ? "拉取中…" : "拉取模型"}
                </button>
              ) : null}
            </div>

            <div className="cblas-row cblas-row--top">
              <span className="cblas-label">测试内容</span>
              <textarea
                className="cblas-textarea"
                rows={3}
                value={probePrompt}
                placeholder="留空则发送：请只回复两个字：你好"
                onChange={(event) => setProbePrompt(event.target.value)}
                disabled={probing}
              />
            </div>

            <div className="cblas-row">
              <button
                type="button"
                className="cblas-button cblas-button--primary"
                onClick={() => void handleProbe()}
                disabled={probing || !probeModel}
              >
                {probing ? "测试中…" : "开始测试"}
              </button>
              <button
                type="button"
                className="cblas-button"
                onClick={() => setProbeOpen(false)}
                disabled={probing}
              >
                关闭
              </button>
            </div>

            {probeResult ? (
              <div className={probeResult.ok ? "cblas-ok" : "cblas-error"}>
                <div>
                  <strong>{probeResult.model}</strong> · {probeResult.message}
                  {probeResult.totalTokens != null ? ` · ${probeResult.totalTokens} tokens` : ""}
                </div>
                {probeResult.content ? (
                  <div className="cblas-probe-output">{probeResult.content}</div>
                ) : null}
                {probeResult.reasoning ? (
                  <details className="cblas-probe-reasoning">
                    <summary>思考过程</summary>
                    <pre>{probeResult.reasoning}</pre>
                  </details>
                ) : null}
              </div>
            ) : null}
          </div>
        </div>
      ) : null}
      </main>
    </div>
  );
}
