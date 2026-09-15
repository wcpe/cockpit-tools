export type WorkbuddyActivityScheduleKind =
  | 'checkin'
  | 'catTravel'
  | 'activityReport'
  | 'schoolSeason'
  | 'nightCat'
  | 'tokenKeepalive';

export interface WorkbuddyGrowthTask {
  taskCode: string;
  name: string;
  description: string;
  status: string;
  current: number;
  target: number;
  rewardCredit: number;
  rewardEnergy: number;
  unforgeable: boolean;
  reason: string;
}

export interface WorkbuddyTravelStatus {
  state: string;
  dailyLimitReached: boolean;
  raw?: unknown;
}

export interface WorkbuddyActivityOverview {
  ok: boolean;
  realm: 'cn' | 'global';
  energy: number;
  streakDays: number;
  travel: WorkbuddyTravelStatus;
  tasks: WorkbuddyGrowthTask[];
  message: string;
}

export interface WorkbuddyActivityRunLog {
  ok: boolean;
  accountId: string;
  label: string;
  earnedCredit: number;
  logs: string[];
}

export interface WorkbuddyActivityLogEntry {
  id: string;
  timestamp: string;
  source: 'schedule' | 'manual' | string;
  kind: string;
  kindLabel: string;
  accountId: string;
  accountLabel: string;
  ok: boolean;
  earnedCredit: number;
  message: string;
  lines: string[];
}

export interface WorkbuddyActivityScheduleStatus {
  enabled: boolean;
  checkinEnabled: boolean;
  catTravelEnabled: boolean;
  activityReportEnabled: boolean;
  schoolSeasonEnabled: boolean;
  nightCatEnabled: boolean;
  tokenKeepaliveEnabled: boolean;
  lastRunAt?: string | null;
  lastSummary?: string | null;
}

export interface WorkbuddyActivityScheduleUpdate {
  enabled?: boolean;
  checkinEnabled?: boolean;
  catTravelEnabled?: boolean;
  activityReportEnabled?: boolean;
  schoolSeasonEnabled?: boolean;
  nightCatEnabled?: boolean;
  tokenKeepaliveEnabled?: boolean;
}

export interface WorkbuddyActivityOverviewAllResult {
  items: Array<{
    accountId: string;
    overview: WorkbuddyActivityOverview;
  }>;
  cacheAccountCount: number;
  cacheNewestAt?: string | null;
}

export type WorkbuddyActivityCenterTab =
  | 'growth'
  | 'cat'
  | 'nightCat'
  | 'activityReport'
  | 'schedule'
  | 'logs';

export const WORKBUDDY_ACTIVITY_CENTER_TABS: Array<{
  id: WorkbuddyActivityCenterTab;
  kind?: WorkbuddyActivityScheduleKind;
  titleKey: string;
  titleDefault: string;
  descKey: string;
  descDefault: string;
  hours: string;
  cnOnly: boolean;
}> = [
  {
    id: 'growth',
    kind: 'schoolSeason',
    titleKey: 'workbuddy.activity.growth',
    titleDefault: '成长任务',
    descKey: 'workbuddy.activity.growthDesc',
    descDefault: '接取并点亮成长/积分任务后自动领奖',
    hours: '12:00',
    cnOnly: true,
  },
  {
    id: 'cat',
    kind: 'catTravel',
    titleKey: 'workbuddy.activity.cat',
    titleDefault: '猫猫旅行',
    descKey: 'workbuddy.activity.catDesc',
    descDefault: '派出旅行或领取归来奖励',
    hours: '09:00 / 21:00',
    cnOnly: true,
  },
  {
    id: 'nightCat',
    kind: 'nightCat',
    titleKey: 'workbuddy.activity.nightCat',
    titleDefault: '夜猫子',
    descKey: 'workbuddy.activity.nightCatDesc',
    descDefault: '夜窗内完成 black_cat 任务',
    hours: '01:00',
    cnOnly: true,
  },
  {
    id: 'activityReport',
    kind: 'activityReport',
    titleKey: 'workbuddy.activity.activityReport',
    titleDefault: '活跃上报',
    descKey: 'workbuddy.activity.activityReportDesc',
    descDefault: '对话事件连发，点亮连登与对话任务',
    hours: '10:00',
    cnOnly: true,
  },
  {
    id: 'schedule',
    titleKey: 'workbuddy.activity.schedule',
    titleDefault: '活动调度',
    descKey: 'workbuddy.activity.scheduleDesc',
    descDefault: '六类任务独立开关与执行状态',
    hours: '—',
    cnOnly: false,
  },
  {
    id: 'logs',
    titleKey: 'workbuddy.activity.logs',
    titleDefault: '活动日志',
    descKey: 'workbuddy.activity.logsDesc',
    descDefault: '活动执行与签到记录（按类型/来源筛选）',
    hours: '—',
    cnOnly: false,
  },
];
