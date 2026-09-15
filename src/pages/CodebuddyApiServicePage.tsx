import { useCallback, useEffect, useMemo, useState } from "react";
import { Download, Play, RefreshCw, Square, Wand2, X, Zap } from "lucide-react";
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
  CodebuddyProbeResult,
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

  const selectedRefs: CodebuddyLocalAccessAccountRef[] = collection?.accounts ?? [];

  const isSelected = (platform: CodebuddyLocalAccessPlatform, accountId: string) =>
    selectedRefs.some((entry) => entry.platform === platform && entry.accountId === accountId);

  const toggleAccount = (option: CodebuddyLocalAccessAccountOption) => {
    if (!collection) {
      return;
    }
    const next = isSelected(option.platform, option.accountId)
      ? selectedRefs.filter(
          (entry) => !(entry.platform === option.platform && entry.accountId === option.accountId),
        )
      : [...selectedRefs, { platform: option.platform, accountId: option.accountId, label: option.label }];
    void run(() =>
      service.saveCodebuddyLocalAccess({ accounts: next, enabled: collection.enabled }),
    );
  };

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
        <h3>账号（{selectedRefs.length} 个已选）</h3>
        {groups.length === 0 ? (
          <p className="cblas-empty">
            未检测到 CodeBuddy / WorkBuddy 账号。请先在对应的管理页面登录账号。
          </p>
        ) : (
          groups.map((group) => (
            <div className="cblas-account-group" key={group.platform}>
              <div className="cblas-account-group-title">{group.name}</div>
              {group.accounts.map((option) => (
                <label className="cblas-account" key={`${option.platform}-${option.accountId}`}>
                  <input
                    type="checkbox"
                    checked={isSelected(option.platform, option.accountId)}
                    onChange={() => toggleAccount(option)}
                    disabled={busy}
                  />
                  <span>{option.label || option.accountId}</span>
                  <span className="cblas-account-meta">
                    {option.tokenAvailable ? (
                      formatExpiry(option.expiresAtMs)
                    ) : (
                      <span className="cblas-token-missing">缺少令牌</span>
                    )}
                  </span>
                </label>
              ))}
            </div>
          ))
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
