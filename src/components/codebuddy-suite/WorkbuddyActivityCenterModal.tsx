import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { listen } from '@tauri-apps/api/event';
import {
  X,
  ChevronLeft,
  Loader2,
  Play,
  RefreshCw,
  Sparkles,
  Cat,
  Moon,
  Activity,
  Clock,
  CheckCircle,
  XCircle,
  Globe,
  FileText,
  Gift,
  Smartphone,
  GraduationCap,
  Zap,
  Trash2,
} from 'lucide-react';
import { useEscClose } from '../../hooks/useEscClose';
import type { CodebuddySuiteAccountBase } from '../../types/codebuddy-suite';
import {
  WORKBUDDY_ACTIVITY_CENTER_TABS,
  type WorkbuddyActivityCenterTab,
  type WorkbuddyActivityOverview,
  type WorkbuddyActivityRunLog,
  type WorkbuddyActivityScheduleKind,
  type WorkbuddyActivityScheduleStatus,
} from '../../types/workbuddyActivity';
import {
  getWorkbuddyActivityOverviewAll,
  getWorkbuddyActivitySchedule,
  getWorkbuddyActivityCachedRuns,
  refreshWorkbuddyActivityAccount,
  refreshWorkbuddyActivityAllSlow,
  runWorkbuddyActivityCatTravel,
  runWorkbuddyActivityDailyAll,
  runWorkbuddyActivityGrowth,
  runWorkbuddyActivityKind,
  runWorkbuddyActivityKindAll,
  runWorkbuddyActivityNightCat,
  runWorkbuddyActivityReport,
  runWorkbuddyActivityScheduleNow,
  updateWorkbuddyActivitySchedule,
  clearWorkbuddyActivityOverviewCache,
} from '../../services/workbuddyActivityService';
import { WorkbuddyActivityLogsPanel } from './WorkbuddyActivityLogsPanel';

export const WORKBUDDY_ACTIVITY_SCHEDULE_CHANGED_EVENT = 'workbuddy-activity-schedule-changed';

const TAB_ICONS: Record<WorkbuddyActivityCenterTab, typeof Sparkles> = {
  growth: Sparkles,
  cat: Cat,
  nightCat: Moon,
  activityReport: Activity,
  streakRewards: Gift,
  schoolSeason: GraduationCap,
  miniprogram: Smartphone,
  schedule: Clock,
  logs: FileText,
};

function isCnAccount(account: CodebuddySuiteAccountBase): boolean {
  const domain = (account as { domain?: string | null }).domain ?? '';
  const value = String(domain || '').toLowerCase();
  if (!value) return true;
  if (value.includes('workbuddy.ai') || value.includes('codebuddy.ai')) return false;
  return true;
}

function resolveRun(
  tab: WorkbuddyActivityCenterTab,
  accountId: string,
): Promise<WorkbuddyActivityRunLog> {
  switch (tab) {
    case 'growth':
      return runWorkbuddyActivityGrowth(accountId);
    case 'cat':
      return runWorkbuddyActivityCatTravel(accountId);
    case 'nightCat':
      return runWorkbuddyActivityNightCat(accountId);
    case 'activityReport':
      return runWorkbuddyActivityReport(accountId);
    case 'streakRewards':
      return runWorkbuddyActivityKind('streakRewards', accountId);
    case 'schoolSeason':
      return runWorkbuddyActivityKind('schoolSeason', accountId);
    case 'miniprogram':
      return runWorkbuddyActivityKind('miniprogram', accountId);
    default:
      return Promise.reject(new Error(`unsupported tab: ${tab}`));
  }
}

interface WorkbuddyActivityCenterModalProps {
  accounts: CodebuddySuiteAccountBase[];
  onClose: () => void;
  onCheckinComplete?: () => void;
  getDisplayEmail?: (account: CodebuddySuiteAccountBase) => string;
}

export function WorkbuddyActivityCenterModal({
  accounts,
  getDisplayEmail,
  onClose,
}: WorkbuddyActivityCenterModalProps) {
  const { t } = useTranslation();
  useEscClose(true, onClose);
  const resolveLabel = useCallback(
    (account: CodebuddySuiteAccountBase) => {
      if (getDisplayEmail) return getDisplayEmail(account);
      const email = (account as { email?: string }).email;
      const nickname = (account as { nickname?: string }).nickname;
      return nickname || email || account.id;
    },
    [getDisplayEmail],
  );

  const [activeTab, setActiveTab] = useState<WorkbuddyActivityCenterTab>('growth');
  const [overview, setOverview] = useState<Record<string, WorkbuddyActivityOverview>>({});
  const [runningId, setRunningId] = useState<string | null>(null);
  const [refreshingId, setRefreshingId] = useState<string | null>(null);
  const [runLogs, setRunLogs] = useState<Record<string, WorkbuddyActivityRunLog>>({});
  const [schedule, setSchedule] = useState<WorkbuddyActivityScheduleStatus | null>(null);
  const [scheduleSaving, setScheduleSaving] = useState(false);
  const [scheduleRunning, setScheduleRunning] = useState<WorkbuddyActivityScheduleKind | null>(
    null,
  );
  const [overviewLoading, setOverviewLoading] = useState(false);
  const [batchRunning, setBatchRunning] = useState(false);
  const [batchProgress, setBatchProgress] = useState<string | null>(null);
  const [cacheInfo, setCacheInfo] = useState<{ count: number; newestAt?: string | null } | null>(
    null,
  );

  const loadSchedule = useCallback(async () => {
    try {
      setSchedule(await getWorkbuddyActivitySchedule());
    } catch (err) {
      console.warn('[WorkbuddyActivity] 读取调度配置失败:', err);
    }
  }, []);

  /** 打开界面：优先读缓存，不强制打接口。 */
  const loadOverview = useCallback(async (force = false) => {
    setOverviewLoading(true);
    try {
      const result = await getWorkbuddyActivityOverviewAll(force);
      const next: Record<string, WorkbuddyActivityOverview> = {};
      for (const item of result.items) {
        next[item.accountId] = item.overview;
      }
      setOverview((prev) => (force ? next : { ...prev, ...next }));
      setCacheInfo({
        count: result.cacheAccountCount,
        newestAt: result.cacheNewestAt,
      });
    } catch (err) {
      console.warn('[WorkbuddyActivity] 拉取总览失败', err);
    } finally {
      setOverviewLoading(false);
    }
  }, []);

  /** 回填上次执行结果，避免重开界面看起来“空了”。 */
  const loadCachedRuns = useCallback(async () => {
    try {
      const cached = await getWorkbuddyActivityCachedRuns();
      const next: Record<string, WorkbuddyActivityRunLog> = {};
      for (const item of cached) {
        // 同账号保留最近一条（list 已按时间倒序，后写覆盖先写时跳过已有）
        if (!next[item.log.accountId]) {
          next[item.log.accountId] = item.log;
        }
      }
      setRunLogs((prev) => ({ ...next, ...prev }));
    } catch (err) {
      console.warn('[WorkbuddyActivity] 读执行结果缓存失败', err);
    }
  }, []);

  /** 单账号强制刷新总览。 */
  const refreshOneAccount = useCallback(
    async (accountId: string) => {
      if (refreshingId) return;
      setRefreshingId(accountId);
      try {
        const overview = await refreshWorkbuddyActivityAccount(accountId);
        setOverview((prev) => ({ ...prev, [accountId]: overview }));
      } catch (err) {
        console.warn('[WorkbuddyActivity] 单号刷新失败', err);
      } finally {
        setRefreshingId(null);
      }
    },
    [refreshingId],
  );

  /** 全账号慢刷：账号间隔约 1.5s，显示进度。 */
  const refreshAllSlow = useCallback(async () => {
    if (overviewLoading || batchRunning) return;
    setOverviewLoading(true);
    setBatchProgress(t('workbuddy.activity.slowRefreshStart', '全量慢刷中…'));
    try {
      const result = await refreshWorkbuddyActivityAllSlow();
      const next: Record<string, WorkbuddyActivityOverview> = {};
      for (const item of result.items) {
        next[item.accountId] = item.overview;
      }
      setOverview(next);
      setCacheInfo({
        count: result.cacheAccountCount,
        newestAt: result.cacheNewestAt,
      });
      setBatchProgress(
        t('workbuddy.activity.slowRefreshDone', '慢刷完成 · {{count}} 账号', {
          count: result.items.length,
        }),
      );
    } catch (err) {
      setBatchProgress(err instanceof Error ? err.message : String(err));
    } finally {
      setOverviewLoading(false);
    }
  }, [overviewLoading, batchRunning, t]);

  useEffect(() => {
    void loadSchedule();
    void loadOverview(false);
    void loadCachedRuns();
    let disposed = false;
    let unlisten: (() => void) | undefined;
    void listen(WORKBUDDY_ACTIVITY_SCHEDULE_CHANGED_EVENT, () => {
      if (!disposed) void loadSchedule();
    })
      .then((stop) => {
        if (disposed) stop();
        else unlisten = stop;
      })
      .catch(() => undefined);
    return () => {
      disposed = true;
      unlisten?.();
    };
  }, [loadSchedule, loadOverview, loadCachedRuns]);

  const activeMeta = useMemo(
    () => WORKBUDDY_ACTIVITY_CENTER_TABS.find((item) => item.id === activeTab)!,
    [activeTab],
  );

  const runAccount = async (accountId: string) => {
    if (activeTab === 'schedule' || activeTab === 'logs') return;
    setRunningId(accountId);
    try {
      const result = await resolveRun(activeTab, accountId);
      setRunLogs((prev) => ({ ...prev, [accountId]: result }));
      if (
        activeTab === 'growth' ||
        activeTab === 'cat' ||
        activeTab === 'nightCat' ||
        activeTab === 'streakRewards' ||
        activeTab === 'schoolSeason' ||
        activeTab === 'miniprogram' ||
        activeTab === 'activityReport'
      ) {
        // 只刷新该账号，避免全量再打一轮
        void refreshOneAccount(accountId);
      }
    } catch (err) {
      setRunLogs((prev) => ({
        ...prev,
        [accountId]: {
          ok: false,
          accountId,
          label: accountId,
          earnedCredit: 0,
          logs: [err instanceof Error ? err.message : String(err)],
        },
      }));
    } finally {
      setRunningId(null);
    }
  };

  const batchKind = useMemo(() => {
    switch (activeTab) {
      case 'growth':
        return 'growth' as const;
      case 'cat':
        return 'catTravel' as const;
      case 'nightCat':
        return 'nightCat' as const;
      case 'activityReport':
        return 'activityReport' as const;
      case 'streakRewards':
        return 'streakRewards' as const;
      case 'schoolSeason':
        return 'schoolSeason' as const;
      case 'miniprogram':
        return 'miniprogram' as const;
      default:
        return null;
    }
  }, [activeTab]);

  const runAllAccounts = async () => {
    if (!batchKind || batchRunning) return;
    setBatchRunning(true);
    setBatchProgress(t('workbuddy.activity.batchRunning', '批量执行中…'));
    try {
      const results = await runWorkbuddyActivityKindAll(batchKind);
      const next: Record<string, WorkbuddyActivityRunLog> = {};
      let earned = 0;
      let okCount = 0;
      for (const item of results) {
        next[item.accountId] = item;
        earned += item.earnedCredit;
        if (item.ok) okCount += 1;
      }
      setRunLogs((prev) => ({ ...prev, ...next }));
      setBatchProgress(
        t(
          'workbuddy.activity.batchDone',
          '完成 {{ok}}/{{total}} 账号 · 到账 {{earned}} 积分',
          { ok: okCount, total: results.length, earned },
        ),
      );
      // 后端已失效缓存；这里只读缓存补齐未命中账号，不再 force 全量
      void loadOverview(false);
    } catch (err) {
      setBatchProgress(err instanceof Error ? err.message : String(err));
    } finally {
      setBatchRunning(false);
    }
  };

  const runDailyAll = async () => {
    if (batchRunning) return;
    setBatchRunning(true);
    setBatchProgress(t('workbuddy.activity.dailyRunning', '一键日常执行中（签到/猫猫/上报/成长）…'));
    try {
      const results = await runWorkbuddyActivityDailyAll();
      const next: Record<string, WorkbuddyActivityRunLog> = {};
      let earned = 0;
      let okCount = 0;
      for (const item of results) {
        next[item.accountId] = item;
        earned += item.earnedCredit;
        if (item.ok) okCount += 1;
      }
      setRunLogs((prev) => ({ ...prev, ...next }));
      setBatchProgress(
        t(
          'workbuddy.activity.dailyDone',
          '日常完成：{{ok}} 条成功 · 到账 {{earned}} 积分',
          { ok: okCount, earned },
        ),
      );
      void loadOverview(false);
    } catch (err) {
      setBatchProgress(err instanceof Error ? err.message : String(err));
    } finally {
      setBatchRunning(false);
    }
  };

  const handleClearCache = async () => {
    try {
      await clearWorkbuddyActivityOverviewCache();
      setOverview({});
      setCacheInfo({ count: 0, newestAt: null });
      setBatchProgress(t('workbuddy.activity.cacheCleared', '缓存已清空，可用慢刷重新拉取'));
    } catch (err) {
      console.warn(err);
    }
  };

  const patchSchedule = async (patch: Partial<WorkbuddyActivityScheduleStatus>) => {
    if (!schedule) return;
    setScheduleSaving(true);
    try {
      const next = await updateWorkbuddyActivitySchedule({
        enabled: patch.enabled,
        checkinEnabled: patch.checkinEnabled,
        catTravelEnabled: patch.catTravelEnabled,
        activityReportEnabled: patch.activityReportEnabled,
        schoolSeasonEnabled: patch.schoolSeasonEnabled,
        nightCatEnabled: patch.nightCatEnabled,
        tokenKeepaliveEnabled: patch.tokenKeepaliveEnabled,
      });
      setSchedule(next);
    } catch (err) {
      console.warn('[WorkbuddyActivity] 保存调度失败:', err);
    } finally {
      setScheduleSaving(false);
    }
  };

  const runScheduleKind = async (kind: WorkbuddyActivityScheduleKind) => {
    setScheduleRunning(kind);
    try {
      await runWorkbuddyActivityScheduleNow(kind);
      await loadSchedule();
    } catch (err) {
      console.warn('[WorkbuddyActivity] 立即执行失败:', err);
    } finally {
      setScheduleRunning(null);
    }
  };

  const Icon = TAB_ICONS[activeTab];

  return (
    <div className="modal-overlay">
      <div className="modal-content checkin-modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <button
            className="btn btn-secondary icon-only"
            onClick={onClose}
            title={t('common.back', '返回')}
          >
            <ChevronLeft size={14} />
          </button>
          <h2>
            <Icon size={20} /> {t('workbuddy.activity.modalTitle', '活动中心')} - WorkBuddy
          </h2>
          <button className="modal-close" onClick={onClose}>
            <X size={18} />
          </button>
        </div>

        <div className="checkin-modal-toolbar">
          <div className="auto-checkin-tabs-nav" style={{ flexWrap: 'wrap', gap: 4 }}>
            {WORKBUDDY_ACTIVITY_CENTER_TABS.map((item) => {
              const TabIcon = TAB_ICONS[item.id];
              return (
                <button
                  key={item.id}
                  className={`auto-checkin-tab-item ${activeTab === item.id ? 'active' : ''}`}
                  onClick={() => setActiveTab(item.id)}
                  title={t(item.descKey, item.descDefault)}
                >
                  <TabIcon size={13} />
                  {t(item.titleKey, item.titleDefault)}
                </button>
              );
            })}
          </div>
          {activeMeta && activeTab !== 'logs' && activeTab !== 'schedule' ? (
            <div
              style={{
                marginTop: 8,
                fontSize: 12,
                opacity: 0.75,
                display: 'flex',
                gap: 12,
                flexWrap: 'wrap',
                alignItems: 'center',
              }}
            >
              <span>{t(activeMeta.descKey, activeMeta.descDefault)}</span>
              {activeMeta.hours !== '—' ? (
                <span style={{ opacity: 0.8 }}>({activeMeta.hours})</span>
              ) : null}
            </div>
          ) : null}
        </div>

        <div className="modal-body" style={{ maxHeight: '60vh', overflowY: 'auto' }}>
          {activeTab === 'logs' ? (
            <WorkbuddyActivityLogsPanel />
          ) : activeTab !== 'schedule' ? (
            <>
              <div
                style={{
                  display: 'flex',
                  gap: 12,
                  alignItems: 'center',
                  marginBottom: 12,
                  flexWrap: 'wrap',
                }}
              >
                <span style={{ opacity: 0.8 }}>{t(activeMeta.descKey, activeMeta.descDefault)}</span>
                <span className="checkin-stat">
                  <Clock size={14} /> {activeMeta.hours}
                </span>
                <button
                  className="btn btn-secondary icon-only"
                  onClick={() => void loadOverview(false)}
                  title={t('workbuddy.activity.useCache', '读缓存（不打接口）')}
                  disabled={overviewLoading}
                >
                  <RefreshCw size={14} className={overviewLoading ? 'loading-spinner' : ''} />
                </button>
                <button
                  className="btn btn-secondary icon-only"
                  onClick={() => void refreshAllSlow()}
                  title={t(
                    'workbuddy.activity.slowRefresh',
                    '全量慢刷（账号间隔约 1.5s，防风控）',
                  )}
                  disabled={overviewLoading || batchRunning}
                >
                  <Zap size={14} className={overviewLoading ? 'loading-spinner' : ''} />
                </button>
                <button
                  className="btn btn-secondary icon-only"
                  onClick={() => void handleClearCache()}
                  title={t('workbuddy.activity.clearCache', '清空状态缓存')}
                >
                  <Trash2 size={14} />
                </button>
                {cacheInfo ? (
                  <span style={{ fontSize: 12, opacity: 0.7 }}>
                    {t('workbuddy.activity.cacheBadge', '缓存 {{count}} 账号 · 最近 {{time}}', {
                      count: cacheInfo.count,
                      time: cacheInfo.newestAt || t('common.none', '无'),
                    })}
                  </span>
                ) : null}
                {batchKind ? (
                  <button
                    className="btn btn-primary"
                    disabled={batchRunning || overviewLoading}
                    onClick={() => void runAllAccounts()}
                  >
                    {batchRunning ? (
                      <Loader2 size={14} className="loading-spinner" />
                    ) : (
                      <Play size={14} />
                    )}
                    <span style={{ marginLeft: 6 }}>
                      {t('workbuddy.activity.runAll', '一键执行全部账号')}
                    </span>
                  </button>
                ) : null}
                <button
                  className="btn btn-primary"
                  disabled={batchRunning}
                  onClick={() => void runDailyAll()}
                  title={t(
                    'workbuddy.activity.dailyAllHint',
                    '依次对全部国内版账号执行签到/猫猫/上报/成长（含连登奖励）',
                  )}
                >
                  {batchRunning ? (
                    <Loader2 size={14} className="loading-spinner" />
                  ) : (
                    <Zap size={14} />
                  )}
                  <span style={{ marginLeft: 6 }}>
                    {t('workbuddy.activity.dailyAll', '一键日常（全账号）')}
                  </span>
                </button>
              </div>
              {batchProgress ? (
                <div
                  style={{
                    marginBottom: 12,
                    fontSize: 12,
                    opacity: 0.85,
                    padding: '6px 10px',
                    borderRadius: 8,
                    background: 'rgba(59,130,246,0.08)',
                  }}
                >
                  {batchProgress}
                </div>
              ) : null}

              {accounts.length === 0 ? (
                <div style={{ opacity: 0.7 }}>{t('workbuddy.noAccounts', '暂无 WorkBuddy 账号')}</div>
              ) : (
                <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
                  {accounts.map((account) => {
                    const cn = isCnAccount(account);
                    const info = overview[account.id];
                    const log = runLogs[account.id];
                    return (
                      <div
                        key={account.id}
                        style={{
                          border: '1px solid rgba(128,128,128,0.25)',
                          borderRadius: 8,
                          padding: 12,
                        }}
                      >
                        <div
                          style={{
                            display: 'flex',
                            justifyContent: 'space-between',
                            gap: 12,
                            alignItems: 'center',
                            flexWrap: 'wrap',
                          }}
                        >
                          <div style={{ minWidth: 0 }}>
                            <div style={{ fontWeight: 600 }}>{resolveLabel(account)}</div>
                            <div style={{ fontSize: 12, opacity: 0.75 }}>
                              {cn
                                ? info
                                  ? t('workbuddy.activity.overviewLine', '能量 {{energy}} · 连登 {{streak}} · 猫猫 {{travel}}', {
                                      energy: info.energy,
                                      streak: info.streakDays,
                                      travel: info.travel?.state ?? '-',
                                    })
                                  : t('workbuddy.activity.overviewPending', '总览加载中…')
                                : t('workbuddy.activity.globalOnlyKeepalive', '国际版：仅 Token 保活适用')}
                            </div>
                          </div>
                          {cn && (
                            <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                              <button
                                className="btn btn-secondary icon-only"
                                disabled={refreshingId === account.id || runningId === account.id}
                                onClick={() => void refreshOneAccount(account.id)}
                                title={t('workbuddy.activity.refreshOne', '刷新该账号状态')}
                              >
                                <RefreshCw
                                  size={14}
                                  className={refreshingId === account.id ? 'loading-spinner' : ''}
                                />
                              </button>
                              <button
                                className="btn btn-primary"
                                disabled={runningId === account.id}
                                onClick={() => void runAccount(account.id)}
                              >
                                {runningId === account.id ? (
                                  <Loader2 size={14} className="loading-spinner" />
                                ) : (
                                  <Play size={14} />
                                )}
                                {t('workbuddy.activity.runNow', '立即执行')}
                              </button>
                            </div>
                          )}
                        </div>
                        {cn && activeTab === 'growth' && info?.tasks?.length ? (
                          <div
                            style={{
                              marginTop: 8,
                              display: 'flex',
                              flexWrap: 'wrap',
                              gap: 6,
                            }}
                          >
                            {info.tasks.slice(0, 8).map((task) => (
                              <span
                                key={task.taskCode}
                                style={{
                                  fontSize: 12,
                                  padding: '2px 8px',
                                  borderRadius: 999,
                                  background:
                                    task.status === 'claimed'
                                      ? 'rgba(34,197,94,0.15)'
                                      : 'rgba(148,163,184,0.15)',
                                }}
                              >
                                {task.name} · {task.status}
                              </span>
                            ))}
                          </div>
                        ) : null}
                        {cn && (activeTab === 'streakRewards' || activeTab === 'schoolSeason' || activeTab === 'miniprogram') ? (
                          <div style={{ marginTop: 6, fontSize: 12, opacity: 0.7 }}>
                            {activeTab === 'streakRewards'
                              ? t(
                                  'workbuddy.activity.streakRewardsHint',
                                  '今日已处理过则自动跳过（按天幂等）；礼包/补偿/补签/兑换/抽奖一次跑完',
                                )
                              : activeTab === 'schoolSeason'
                                ? t(
                                    'workbuddy.activity.schoolSeasonHint',
                                    '需账号可访问开学季活动；未开放或已完成会静默跳过',
                                  )
                                : t(
                                    'workbuddy.activity.miniprogramHint',
                                    '小程序限定成长任务，缺任务清单时提示未开放',
                                  )}
                          </div>
                        ) : null}
                        {log?.logs?.length ? (
                          <div
                            style={{
                              marginTop: 8,
                              fontSize: 12,
                              opacity: 0.85,
                              whiteSpace: 'pre-wrap',
                              fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
                            }}
                          >
                            {log.ok ? (
                              <CheckCircle size={12} style={{ marginRight: 4 }} />
                            ) : (
                              <XCircle size={12} style={{ marginRight: 4 }} />
                            )}
                            {log.logs.join('\n')}
                            {log.earnedCredit > 0
                              ? `\n${t('workbuddy.activity.earned', '到账积分')}: +${log.earnedCredit}`
                              : ''}
                          </div>
                        ) : null}
                      </div>
                    );
                  })}
                </div>
              )}
            </>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
              <div className="config-form-item toggle-item">
                <div className="toggle-label">
                  <span className="label-title">
                    {t('workbuddy.activity.scheduleMaster', '启用活动调度')}
                  </span>
                  <span className="label-desc">
                    {t(
                      'workbuddy.activity.scheduleMasterDesc',
                      '按官方默认整点自动执行签到/猫猫/上报/开学季/夜猫/保活；国际版仅 Token 保活',
                    )}
                  </span>
                </div>
                <label className="toggle-switch">
                  <input
                    type="checkbox"
                    checked={schedule?.enabled ?? false}
                    disabled={!schedule || scheduleSaving}
                    onChange={(e) => void patchSchedule({ enabled: e.target.checked })}
                  />
                  <span className="slider round"></span>
                </label>
              </div>

              {(
                [
                  ['checkinEnabled', 'checkin', 'workbuddy.activity.kindCheckin', '每日签到', '09:00 / 21:00'],
                  ['catTravelEnabled', 'catTravel', 'workbuddy.activity.kindCat', '猫猫旅行', '09:00 / 21:00'],
                  ['activityReportEnabled', 'activityReport', 'workbuddy.activity.kindReport', '活跃上报', '10:00'],
                  ['schoolSeasonEnabled', 'schoolSeason', 'workbuddy.activity.kindSchool', '开学季任务', '12:00'],
                  ['nightCatEnabled', 'nightCat', 'workbuddy.activity.kindNight', '夜猫子', '01:00'],
                  ['tokenKeepaliveEnabled', 'tokenKeepalive', 'workbuddy.activity.kindKeepalive', 'Token 保活', '22:00'],
                ] as const
              ).map(([field, kind, titleKey, titleDefault, hours]) => (
                <div key={field} className="config-form-item toggle-item">
                  <div className="toggle-label">
                    <span className="label-title">
                      {t(titleKey, titleDefault)}
                      <span style={{ marginLeft: 8, opacity: 0.65, fontWeight: 400 }}>{hours}</span>
                    </span>
                  </div>
                  <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                    <label className="toggle-switch">
                      <input
                        type="checkbox"
                        checked={schedule ? Boolean(schedule[field]) : false}
                        disabled={!schedule || scheduleSaving}
                        onChange={(e) => void patchSchedule({ [field]: e.target.checked })}
                      />
                      <span className="slider round"></span>
                    </label>
                    <button
                      className="btn btn-secondary icon-only"
                      disabled={scheduleRunning === kind}
                      onClick={() => void runScheduleKind(kind)}
                      title={t('workbuddy.activity.runNow', '立即执行')}
                    >
                      {scheduleRunning === kind ? (
                        <Loader2 size={14} className="loading-spinner" />
                      ) : (
                        <Play size={14} />
                      )}
                    </button>
                  </div>
                </div>
              ))}

              <div>
                <h3 className="section-title">
                  {t('workbuddy.activity.scheduleStatus', '调度状态')}
                  {schedule?.lastRunAt ? (
                    <span style={{ marginLeft: 8, fontWeight: 400, opacity: 0.7 }}>
                      {t('workbuddy.activity.lastRunAt', '上次执行 {{time}}', {
                        time: schedule.lastRunAt,
                      })}
                    </span>
                  ) : null}
                </h3>
                {!schedule?.lastSummary ? (
                  <div style={{ opacity: 0.7 }}>
                    {t('workbuddy.activity.noScheduleLogs', '暂无调度日志')}
                  </div>
                ) : (
                  <div style={{ fontSize: 12, opacity: 0.9 }}>{schedule.lastSummary}</div>
                )}
                <button
                  type="button"
                  className="btn btn-secondary"
                  style={{ marginTop: 8 }}
                  onClick={() => setActiveTab('logs')}
                >
                  <FileText size={14} />
                  <span style={{ marginLeft: 6 }}>
                    {t('workbuddy.activity.openLogs', '查看完整活动日志')}
                  </span>
                </button>
              </div>

              <div style={{ fontSize: 12, opacity: 0.65, display: 'flex', gap: 6, alignItems: 'center' }}>
                <Globe size={12} />
                {t(
                  'workbuddy.activity.globalAdaptHint',
                  '国际版账号自动隐藏签到/猫猫/成长等国内活动，仅保留 Token 保活',
                )}
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
