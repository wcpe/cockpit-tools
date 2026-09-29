import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { CheckCircle, Loader2, RefreshCw, Trash2, XCircle } from 'lucide-react';
import type { WorkbuddyActivityLogEntry } from '../../types/workbuddyActivity';
import {
  clearWorkbuddyActivityLogs,
  getWorkbuddyActivityLogs,
} from '../../services/workbuddyActivityService';
import {
  getWorkbuddyAutoCheckinLogsAsync,
  type WorkbuddyAutoCheckinLogRecord,
} from '../../services/workbuddyAutoCheckinService';

type SourceFilter = '' | 'schedule' | 'manual';
type KindFilter = '' | 'checkin' | 'growth' | 'catTravel' | 'nightCat' | 'activityReport' | 'tokenKeepalive';
type ViewTab = 'activity' | 'checkin';

/** 每页条数：避免一次挂载上百个 DOM 节点导致卡死。 */
const PAGE_SIZE = 40;
/** 详情展开最多展示的行数。 */
const MAX_DETAIL_LINES = 80;
/** 签到记录明细最多展示账号数。 */
const MAX_CHECKIN_DETAILS = 30;

const KIND_OPTIONS: Array<{ value: KindFilter; labelKey: string; label: string }> = [
  { value: '', labelKey: 'common.all', label: '全部' },
  { value: 'checkin', labelKey: 'workbuddy.activity.kindCheckin', label: '每日签到' },
  { value: 'growth', labelKey: 'workbuddy.activity.kindSchool', label: '成长/开学季/小程序' },
  { value: 'catTravel', labelKey: 'workbuddy.activity.kindCat', label: '猫猫旅行' },
  { value: 'nightCat', labelKey: 'workbuddy.activity.kindNight', label: '夜猫子' },
  {
    value: 'activityReport',
    labelKey: 'workbuddy.activity.kindReport',
    label: '活跃上报/连登奖励',
  },
  { value: 'tokenKeepalive', labelKey: 'workbuddy.activity.kindKeepalive', label: 'Token 保活' },
];

function truncateLines(lines: string[] | undefined, max: number): string {
  if (!lines?.length) return '';
  if (lines.length <= max) return lines.join('\n');
  return `${lines.slice(0, max).join('\n')}\n…（共 ${lines.length} 行，已截断）`;
}

export function WorkbuddyActivityLogsPanel() {
  const { t } = useTranslation();
  const [viewTab, setViewTab] = useState<ViewTab>('activity');
  const [kindFilter, setKindFilter] = useState<KindFilter>('');
  const [sourceFilter, setSourceFilter] = useState<SourceFilter>('');
  const [activityLogs, setActivityLogs] = useState<WorkbuddyActivityLogEntry[]>([]);
  const [checkinLogs, setCheckinLogs] = useState<WorkbuddyAutoCheckinLogRecord[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [expandedIds, setExpandedIds] = useState<Record<string, boolean>>({});
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [activity, checkin] = await Promise.all([
        getWorkbuddyActivityLogs({
          kind: kindFilter || undefined,
          source: sourceFilter || undefined,
        }),
        getWorkbuddyAutoCheckinLogsAsync(),
      ]);
      setActivityLogs(activity);
      setCheckinLogs(checkin);
      setVisibleCount(PAGE_SIZE);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, [kindFilter, sourceFilter]);

  useEffect(() => {
    void load();
  }, [load]);

  // 筛选变化时重置分页
  useEffect(() => {
    setVisibleCount(PAGE_SIZE);
  }, [kindFilter, sourceFilter, viewTab]);

  const handleClearActivity = async () => {
    if (!window.confirm(t('workbuddy.activity.clearLogsConfirm', '确定清空活动执行日志？'))) {
      return;
    }
    try {
      await clearWorkbuddyActivityLogs();
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const stats = useMemo(() => {
    const ok = activityLogs.filter((e) => e.ok).length;
    return { total: activityLogs.length, ok, fail: activityLogs.length - ok };
  }, [activityLogs]);

  const visibleActivityLogs = useMemo(
    () => activityLogs.slice(0, visibleCount),
    [activityLogs, visibleCount],
  );
  const visibleCheckinLogs = useMemo(
    () => checkinLogs.slice(0, visibleCount),
    [checkinLogs, visibleCount],
  );
  const hasMore =
    viewTab === 'activity'
      ? activityLogs.length > visibleCount
      : checkinLogs.length > visibleCount;

  const toggleExpand = useCallback((id: string) => {
    setExpandedIds((prev) => ({ ...prev, [id]: !prev[id] }));
  }, []);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div
        style={{
          display: 'flex',
          flexWrap: 'wrap',
          gap: 8,
          alignItems: 'center',
        }}
      >
        <div className="auto-checkin-tabs-nav" style={{ flexWrap: 'wrap', gap: 6 }}>
          <button
            className={`auto-checkin-tab-item ${viewTab === 'activity' ? 'active' : ''}`}
            onClick={() => setViewTab('activity')}
          >
            {t('workbuddy.activity.logsActivity', '活动执行')}
          </button>
          <button
            className={`auto-checkin-tab-item ${viewTab === 'checkin' ? 'active' : ''}`}
            onClick={() => setViewTab('checkin')}
          >
            {t('workbuddy.checkin.tabHistory', '自动签到记录')}
          </button>
        </div>
        <button
          className="btn btn-secondary icon-only"
          onClick={() => void load()}
          title={t('common.refresh', '刷新')}
          disabled={loading}
        >
          <RefreshCw size={14} className={loading ? 'loading-spinner' : ''} />
        </button>
        {viewTab === 'activity' ? (
          <button
            className="btn btn-secondary icon-only"
            onClick={() => void handleClearActivity()}
            title={t('common.clear', '清空')}
          >
            <Trash2 size={14} />
          </button>
        ) : null}
      </div>

      {viewTab === 'activity' ? (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, alignItems: 'center' }}>
          <select
            className="cblas-select"
            style={{ minWidth: 120 }}
            value={kindFilter}
            onChange={(e) => setKindFilter(e.target.value as KindFilter)}
          >
            {KIND_OPTIONS.map((option) => (
              <option key={option.value || 'all'} value={option.value}>
                {t(option.labelKey, option.label)}
              </option>
            ))}
          </select>
          <select
            className="cblas-select"
            style={{ minWidth: 120 }}
            value={sourceFilter}
            onChange={(e) => setSourceFilter(e.target.value as SourceFilter)}
          >
            <option value="">{t('workbuddy.activity.sourceAll', '全部来源')}</option>
            <option value="schedule">{t('workbuddy.activity.sourceSchedule', '调度')}</option>
            <option value="manual">{t('workbuddy.activity.sourceManual', '手动')}</option>
          </select>
          <span style={{ fontSize: 12, opacity: 0.75 }}>
            {t('workbuddy.activity.logsStats', '共 {{total}} 条 · 成功 {{ok}} · 失败 {{fail}}', {
              total: stats.total,
              ok: stats.ok,
              fail: stats.fail,
            })}
          </span>
        </div>
      ) : null}

      {error ? <div className="cblas-error">{error}</div> : null}

      {viewTab === 'activity' ? (
        loading && activityLogs.length === 0 ? (
          <div style={{ opacity: 0.7, display: 'flex', gap: 8, alignItems: 'center' }}>
            <Loader2 size={14} className="loading-spinner" />
            {t('common.loading', '加载中…')}
          </div>
        ) : activityLogs.length === 0 ? (
          <div style={{ opacity: 0.7 }}>{t('workbuddy.activity.noLogs', '暂无活动日志')}</div>
        ) : (
          <>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              {visibleActivityLogs.map((entry) => {
                const expanded = expandedIds[entry.id];
                return (
                  <div
                    key={entry.id}
                    style={{
                      border: '1px solid var(--border, rgba(128,128,128,0.25))',
                      borderRadius: 8,
                      padding: '10px 12px',
                      background: 'var(--bg-card, transparent)',
                    }}
                  >
                    <div
                      style={{
                        display: 'flex',
                        gap: 8,
                        alignItems: 'flex-start',
                        flexWrap: 'wrap',
                      }}
                    >
                      {entry.ok ? (
                        <CheckCircle size={14} style={{ color: '#22c55e', marginTop: 2, flexShrink: 0 }} />
                      ) : (
                        <XCircle size={14} style={{ color: '#ef4444', marginTop: 2, flexShrink: 0 }} />
                      )}
                      <div style={{ minWidth: 0, flex: 1 }}>
                        <div style={{ fontSize: 13, fontWeight: 600 }}>
                          {entry.kindLabel}
                          <span style={{ marginLeft: 8, fontWeight: 400, opacity: 0.7 }}>
                            {entry.accountLabel}
                          </span>
                          <span
                            style={{
                              marginLeft: 8,
                              fontSize: 11,
                              padding: '1px 6px',
                              borderRadius: 999,
                              background:
                                entry.source === 'manual'
                                  ? 'rgba(59,130,246,0.15)'
                                  : 'rgba(148,163,184,0.15)',
                            }}
                          >
                            {entry.source === 'manual'
                              ? t('workbuddy.activity.sourceManual', '手动')
                              : t('workbuddy.activity.sourceSchedule', '调度')}
                          </span>
                          {entry.earnedCredit > 0 ? (
                            <span style={{ marginLeft: 8, fontSize: 12, color: '#16a34a' }}>
                              +{entry.earnedCredit}
                            </span>
                          ) : null}
                        </div>
                        <div style={{ fontSize: 12, opacity: 0.8, marginTop: 2 }}>
                          [{entry.timestamp}] {entry.message}
                        </div>
                        {entry.lines?.length > 1 ? (
                          <>
                            <button
                              type="button"
                              className="btn btn-secondary"
                              style={{ marginTop: 6, fontSize: 12, padding: '2px 8px' }}
                              onClick={() => toggleExpand(entry.id)}
                            >
                              {expanded
                                ? t('common.collapse', '收起')
                                : t('workbuddy.activity.showDetail', '展开详情')}
                            </button>
                            {expanded ? (
                              <pre
                                style={{
                                  margin: '6px 0 0',
                                  padding: 8,
                                  borderRadius: 6,
                                  background: 'var(--bg-secondary, rgba(0,0,0,0.04))',
                                  fontSize: 12,
                                  whiteSpace: 'pre-wrap',
                                  wordBreak: 'break-word',
                                  maxHeight: 240,
                                  overflow: 'auto',
                                }}
                              >
                                {truncateLines(entry.lines, MAX_DETAIL_LINES)}
                              </pre>
                            ) : null}
                          </>
                        ) : null}
                      </div>
                    </div>
                  </div>
                );
              })}
            </div>
            {hasMore ? (
              <button
                type="button"
                className="btn btn-secondary"
                onClick={() => setVisibleCount((n) => n + PAGE_SIZE)}
              >
                {t('workbuddy.activity.loadMore', '加载更多（已显示 {{shown}} / {{total}}）', {
                  shown: Math.min(visibleCount, activityLogs.length),
                  total: activityLogs.length,
                })}
              </button>
            ) : null}
          </>
        )
      ) : checkinLogs.length === 0 ? (
        <div style={{ opacity: 0.7 }}>{t('workbuddy.checkin.noLogs', '暂无自动签到记录')}</div>
      ) : (
        <>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            {visibleCheckinLogs.map((log) => (
              <div
                key={log.id}
                style={{
                  border: '1px solid var(--border, rgba(128,128,128,0.25))',
                  borderRadius: 8,
                  padding: '10px 12px',
                }}
              >
                <div style={{ fontSize: 13, fontWeight: 600 }}>
                  [{log.timestamp}]{' '}
                  {log.status === 'success'
                    ? t('workbuddy.checkin.checkedIn', '已签到')
                    : log.status === 'partial'
                      ? t('workbuddy.activity.partial', '部分成功')
                      : log.status}
                  <span style={{ marginLeft: 8, fontWeight: 400, opacity: 0.75 }}>
                    {t('workbuddy.activity.checkinStats', '成功 {{ok}} · 已签 {{already}} · 失败 {{fail}}', {
                      ok: log.successCount,
                      already: log.alreadyCheckedCount,
                      fail: log.failedCount,
                    })}
                  </span>
                </div>
                {log.details?.length ? (
                  <div style={{ marginTop: 6, fontSize: 12, opacity: 0.85 }}>
                    {log.details.slice(0, MAX_CHECKIN_DETAILS).map((detail) => (
                      <div key={detail.accountId}>
                        {detail.email || detail.accountId}: {detail.status}
                        {detail.message ? ` — ${detail.message}` : ''}
                      </div>
                    ))}
                    {log.details.length > MAX_CHECKIN_DETAILS ? (
                      <div style={{ opacity: 0.7 }}>
                        {t('workbuddy.activity.moreAccounts', '… 另有 {{count}} 个账号未展开', {
                          count: log.details.length - MAX_CHECKIN_DETAILS,
                        })}
                      </div>
                    ) : null}
                  </div>
                ) : null}
              </div>
            ))}
          </div>
          {hasMore ? (
            <button
              type="button"
              className="btn btn-secondary"
              onClick={() => setVisibleCount((n) => n + PAGE_SIZE)}
            >
              {t('workbuddy.activity.loadMore', '加载更多（已显示 {{shown}} / {{total}}）', {
                shown: Math.min(visibleCount, checkinLogs.length),
                total: checkinLogs.length,
              })}
            </button>
          ) : null}
        </>
      )}
    </div>
  );
}
