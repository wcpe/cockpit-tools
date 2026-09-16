import { invoke } from '@tauri-apps/api/core';
import type {
  WorkbuddyActivityLogEntry,
  WorkbuddyActivityOverview,
  WorkbuddyActivityOverviewAllResult,
  WorkbuddyActivityRunKind,
  WorkbuddyActivityRunLog,
  WorkbuddyActivityScheduleKind,
  WorkbuddyActivityScheduleStatus,
  WorkbuddyActivityScheduleUpdate,
} from '../types/workbuddyActivity';

export async function getWorkbuddyActivityOverview(
  accountId: string,
): Promise<WorkbuddyActivityOverview> {
  return await invoke<WorkbuddyActivityOverview>('workbuddy_activity_overview', { accountId });
}

export async function getWorkbuddyActivityOverviewAll(
  forceRefresh = false,
): Promise<WorkbuddyActivityOverviewAllResult> {
  return await invoke<WorkbuddyActivityOverviewAllResult>('workbuddy_activity_overview_all', {
    forceRefresh,
  });
}

/** 单账号强制刷新总览。 */
export async function refreshWorkbuddyActivityAccount(
  accountId: string,
): Promise<WorkbuddyActivityOverview> {
  return await invoke<WorkbuddyActivityOverview>('workbuddy_activity_refresh_account', {
    accountId,
  });
}

/** 全账号慢刷总览（账号间隔约 1.5s）。 */
export async function refreshWorkbuddyActivityAllSlow(): Promise<WorkbuddyActivityOverviewAllResult> {
  return await invoke<WorkbuddyActivityOverviewAllResult>('workbuddy_activity_refresh_all_slow');
}

/** 上次执行结果缓存（可选按 kind 过滤）。 */
export interface WorkbuddyCachedRunLog {
  log: WorkbuddyActivityRunLog;
  kind: string;
  ranAt: string;
  ranAtTs: number;
}

export async function getWorkbuddyActivityCachedRuns(
  kind?: string,
): Promise<WorkbuddyCachedRunLog[]> {
  return await invoke<WorkbuddyCachedRunLog[]>('workbuddy_activity_get_cached_runs', { kind });
}

export async function runWorkbuddyActivityKindAll(
  kind: WorkbuddyActivityRunKind,
): Promise<WorkbuddyActivityRunLog[]> {
  return await invoke<WorkbuddyActivityRunLog[]>('workbuddy_activity_run_kind_all', { kind });
}

export async function runWorkbuddyActivityDailyAll(): Promise<WorkbuddyActivityRunLog[]> {
  return await invoke<WorkbuddyActivityRunLog[]>('workbuddy_activity_run_daily_all');
}

export async function clearWorkbuddyActivityOverviewCache(): Promise<void> {
  await invoke('workbuddy_activity_clear_overview_cache');
}

export async function runWorkbuddyActivityGrowth(
  accountId: string,
): Promise<WorkbuddyActivityRunLog> {
  return await invoke<WorkbuddyActivityRunLog>('workbuddy_activity_run_growth', { accountId });
}

export async function runWorkbuddyActivityCatTravel(
  accountId: string,
): Promise<WorkbuddyActivityRunLog> {
  return await invoke<WorkbuddyActivityRunLog>('workbuddy_activity_cat_travel', { accountId });
}

export async function runWorkbuddyActivityCheckin(accountId: string): Promise<{
  success: boolean;
  message?: string;
  credit?: number;
  streak_days?: number;
  is_streak_day?: boolean;
  reward?: unknown;
}> {
  return await invoke('workbuddy_activity_run_checkin', { accountId });
}

export async function runWorkbuddyActivityReport(
  accountId: string,
): Promise<WorkbuddyActivityRunLog> {
  return await invoke<WorkbuddyActivityRunLog>('workbuddy_activity_run_activity_report', {
    accountId,
  });
}

/** 按 kind 执行单个账号（streakRewards / schoolSeason / miniprogram 等）。 */
export async function runWorkbuddyActivityKind(
  kind: string,
  accountId: string,
): Promise<WorkbuddyActivityRunLog> {
  return await invoke<WorkbuddyActivityRunLog>('workbuddy_activity_run_kind', {
    kind,
    accountId,
  });
}

export async function runWorkbuddyActivityNightCat(
  accountId: string,
): Promise<WorkbuddyActivityRunLog> {
  return await invoke<WorkbuddyActivityRunLog>('workbuddy_activity_run_night_cat', { accountId });
}

export async function runWorkbuddyActivityTask(
  accountId: string,
  taskCode: string,
): Promise<WorkbuddyActivityRunLog> {
  return await invoke<WorkbuddyActivityRunLog>('workbuddy_activity_run_task', {
    accountId,
    taskCode,
  });
}

export async function getWorkbuddyActivitySchedule(): Promise<WorkbuddyActivityScheduleStatus> {
  return await invoke<WorkbuddyActivityScheduleStatus>('workbuddy_activity_get_schedule');
}

export async function updateWorkbuddyActivitySchedule(
  update: WorkbuddyActivityScheduleUpdate,
): Promise<WorkbuddyActivityScheduleStatus> {
  return await invoke<WorkbuddyActivityScheduleStatus>('workbuddy_activity_update_schedule', {
    enabled: update.enabled ?? null,
    checkinEnabled: update.checkinEnabled ?? null,
    catTravelEnabled: update.catTravelEnabled ?? null,
    activityReportEnabled: update.activityReportEnabled ?? null,
    schoolSeasonEnabled: update.schoolSeasonEnabled ?? null,
    nightCatEnabled: update.nightCatEnabled ?? null,
    tokenKeepaliveEnabled: update.tokenKeepaliveEnabled ?? null,
  });
}

export async function runWorkbuddyActivityScheduleNow(
  kind: WorkbuddyActivityScheduleKind,
): Promise<void> {
  await invoke('workbuddy_activity_run_schedule_now', { kind });
}

export async function getWorkbuddyActivityLogs(options?: {
  kind?: string;
  source?: string;
}): Promise<WorkbuddyActivityLogEntry[]> {
  return await invoke<WorkbuddyActivityLogEntry[]>('workbuddy_activity_get_logs', {
    kind: options?.kind || null,
    source: options?.source || null,
  });
}

export async function clearWorkbuddyActivityLogs(): Promise<void> {
  await invoke('workbuddy_activity_clear_logs');
}
