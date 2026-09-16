import { Fragment, useCallback, useEffect, useMemo, useState } from "react";
import { Activity, Download, Play, RefreshCw, Square, Wand2, X, Zap } from "lucide-react";
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
  const [copied, setCopied] = useState<"base" | "key" | null>(null);
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
  const [runtimeView, setRuntimeView] = useState<"ledger" | "requests" | "usage">("ledger");
  const [requestPage, setRequestPage] = useState(0);
  const [requestPageSize, setRequestPageSize] = useState(20);
  const [requestTotal, setRequestTotal] = useState(0);
  const [requestRecords, setRequestRecords] = useState<CodebuddyRequestRecord[]>([]);
  const [requestsLoading, setRequestsLoading] = useState(false);
  const [expandedRequestId, setExpandedRequestId] = useState<string | null>(null);
  const [modelUsage, setModelUsage] = useState<CodebuddyModelUsageRow[]>([]);

  const refresh = useCallback(async () => {
    try {
      const next = await service.getCodebuddyLocalAccessState();
      setState(next);
      setPortDraft(String(next.collection.port));
      if (next.lastError) {
        setError(next.lastError);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const refreshRuntimeStatus = useCallback(async () => {
    if (!state?.running) {
      setRuntimeStatus(null);
      setRuntimeStatusError(null);
      return;
    }
    setRuntimeStatusLoading(true);
    setRuntimeStatusError(null);
    try {
      const next = await service.getCodebuddyRuntimeStatus();
      setRuntimeStatus(next);
      if (next.modelUsage) {
        setModelUsage(next.modelUsage);
      }
    } catch (err) {
      setRuntimeStatusError(err instanceof Error ? err.message : String(err));
    } finally {
      setRuntimeStatusLoading(false);
    }
  }, [state?.running]);

  const loadRequestPage = useCallback(
    async (page: number, pageSize: number) => {
      if (!state?.running) {
        setRequestRecords([]);
        setRequestTotal(0);
        return;
      }
      setRequestsLoading(true);
      setRuntimeStatusError(null);
      try {
        const offset = Math.max(0, page) * pageSize;
        const result = await service.getCodebuddyRuntimeRequests(offset, pageSize);
        setRequestRecords(result.records ?? []);
        setRequestTotal(result.total ?? 0);
        if (result.modelUsage) {
          setModelUsage(result.modelUsage);
        }
        setRequestPage(page);
        setRequestPageSize(pageSize);
      } catch (err) {
        setRuntimeStatusError(err instanceof Error ? err.message : String(err));
      } finally {
        setRequestsLoading(false);
      }
    },
    [state?.running],
  );

  useEffect(() => {
    if (state?.running) {
      void refreshRuntimeStatus();
    } else {
      setRuntimeStatus(null);
    }
  }, [state?.running, refreshRuntimeStatus]);

  useEffect(() => {
    if (runtimeView === "requests" && state?.running) {
      void loadRequestPage(0, requestPageSize);
    }
  }, [runtimeView, state?.running, loadRequestPage, requestPageSize]);

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

  const copy = async (kind: "base" | "key", value: string) => {
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

      <section className="cblas-card">
        <div className="cblas-card-head">
          <h3>
            <Activity size={14} style={{ marginRight: 6, verticalAlign: "middle" }} />
            请求与限流台账
          </h3>
          <div className="cblas-row" style={{ marginBottom: 0 }}>
            <button
              type="button"
              className={`cblas-button ${runtimeView === "ledger" ? "cblas-button--primary" : ""}`}
              onClick={() => setRuntimeView("ledger")}
            >
              限流冷却
            </button>
            <button
              type="button"
              className={`cblas-button ${runtimeView === "requests" ? "cblas-button--primary" : ""}`}
              onClick={() => setRuntimeView("requests")}
            >
              请求流水
            </button>
            <button
              type="button"
              className={`cblas-button ${runtimeView === "usage" ? "cblas-button--primary" : ""}`}
              onClick={() => {
                setRuntimeView("usage");
                void refreshRuntimeStatus();
              }}
            >
              用量统计
            </button>
            <button
              type="button"
              className="cblas-button"
              onClick={() => {
                if (runtimeView === "requests") {
                  void loadRequestPage(requestPage, requestPageSize);
                } else {
                  void refreshRuntimeStatus();
                }
              }}
              disabled={
                (runtimeView === "requests" ? requestsLoading : runtimeStatusLoading) ||
                !state?.running
              }
            >
              <RefreshCw
                size={14}
                className={
                  runtimeView === "requests"
                    ? requestsLoading
                      ? "loading-spinner"
                      : ""
                    : runtimeStatusLoading
                      ? "loading-spinner"
                      : ""
                }
              />
              <span style={{ marginLeft: 6 }}>刷新</span>
            </button>
          </div>
        </div>
        {!state?.running ? (
          <p className="cblas-empty">服务未运行。启动后可查看 6004 模型冷却与请求流水。</p>
        ) : runtimeStatusError ? (
          <div className="cblas-error">{runtimeStatusError}</div>
        ) : runtimeView === "ledger" ? (
          <div>
            <div style={{ fontSize: 12, opacity: 0.75, marginBottom: 8 }}>
              提示词模式：{runtimeStatus?.promptMode || "passthrough"} · 6004 仅冷却触发模型，切换其他模型立即可用
            </div>
            {(runtimeStatus?.rateLimitedModels ?? []).length === 0 ? (
              <p className="cblas-empty">当前没有模型级限流冷却。</p>
            ) : (
              <table className="cblas-table">
                <thead>
                  <tr>
                    <th>账号</th>
                    <th>模型</th>
                    <th>冷却至</th>
                    <th>上游重置</th>
                    <th>原因</th>
                  </tr>
                </thead>
                <tbody>
                  {(runtimeStatus?.rateLimitedModels ?? []).map((item, index) => (
                    <tr key={`${item.accountId || item.id}-${item.model}-${index}`}>
                      <td>{item.accountLabel || item.accountId || item.id || "—"}</td>
                      <td>
                        <span className="cblas-mono">{item.model}</span>
                      </td>
                      <td>{formatTs(item.until)}</td>
                      <td>{formatTs(item.resetAt)}</td>
                      <td>{item.reason || "6004 model rate limit"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
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
                共 {requestTotal} 条 · 每页
              </span>
              <select
                className="cblas-select"
                style={{ width: "auto", minWidth: 72 }}
                value={String(requestPageSize)}
                disabled={requestsLoading}
                onChange={(e) => {
                  const size = Number(e.target.value) || 20;
                  void loadRequestPage(0, size);
                }}
              >
                {[10, 20, 50].map((n) => (
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
                  {requestRecords.map((rec: CodebuddyRequestRecord) => {
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

      <section className="cblas-card">
        <h3>服务</h3>
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

      <section className="cblas-card">
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

      <section className="cblas-card">
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

      <section className="cblas-card">
        <h3>模型（{(state?.modelIds ?? []).length} 个已启用）</h3>
        <div className="cblas-row">
          <button
            type="button"
            className="cblas-button"
            onClick={() => void handleFetchModels()}
            disabled={fetchingModels || !collection || selectedRefs.length === 0}
          >
            <Download size={14} />
            <span style={{ marginLeft: 6 }}>{fetchingModels ? "拉取中…" : "拉取模型"}</span>
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
            模型列表从上游企业接口实时拉取，比内置预设更新
          </span>
        </div>

        {modelsMessage ? <div className="cblas-note">{modelsMessage}</div> : null}

        {fetchedModels.length > 0 ? (
          <div className="cblas-model-table">
            {fetchedModels.map((model) => (
              <div className="cblas-model-row" key={model.id}>
                <div className="cblas-model-main">
                  <span className="cblas-model-id">{model.id}</span>
                  {model.name ? <span className="cblas-model-name">{model.name}</span> : null}
                  {model.cli ? <span className="cblas-model-tag">CLI</span> : null}
                  {model.supportsImages ? <span className="cblas-model-tag">多模态</span> : null}
                  {model.supportsReasoning ? <span className="cblas-model-tag">推理</span> : null}
                </div>
                <div className="cblas-model-meta">
                  {model.contextLength > 0 ? `上下文 ${Math.round(model.contextLength / 1000)}K` : ""}
                  {model.maxOutputTokens > 0 ? ` · 输出 ${Math.round(model.maxOutputTokens / 1000)}K` : ""}
                  {model.efforts.length > 0 ? ` · 档位 ${model.efforts.join("/")}` : ""}
                </div>
                {model.description ? (
                  <div className="cblas-model-desc">{model.description}</div>
                ) : null}
              </div>
            ))}
          </div>
        ) : (
          <div className="cblas-models">
            {(state?.modelIds ?? []).map((model) => (
              <span className="cblas-model-chip" key={model}>
                {model}
              </span>
            ))}
          </div>
        )}
      </section>

      <section className="cblas-card">
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
