//! WorkBuddy 活动中心后台调度器。
//!
//! 对齐官方默认排程（CST 24h）：
//! * 09/21 —— 国内版签到 + 猫猫旅行
//! * 10 —— 活跃上报
//! * 12 —— 开学季/成长任务
//! * 22 —— token 保活
//! * 01 —— 夜猫子（夜窗 23:00–08:00）
//!
//! 六类任务独立开关，配置与日志持久化到 `workbuddy_activity_schedule.json`。

use std::collections::HashMap;
use std::fs;
use std::path::PathBuf;
use std::sync::{Mutex, MutexGuard, OnceLock};
use std::time::Duration;

use chrono::{Datelike, Local, TimeZone, Timelike};
use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Emitter};
use tokio::sync::Notify;

use crate::models::workbuddy::WorkbuddyAccount;
use crate::modules::{
    codebuddy_cn_oauth, config, logger, workbuddy_account, workbuddy_activity,
    workbuddy_activity::{ActivityRunLog, WorkbuddyRealm},
    workbuddy_activity_log::{self as activity_log, ActivityLogKind, ActivityLogSource},
};

const POLL_DELAY: Duration = Duration::from_secs(30);
const INITIAL_SWEEP_DELAY: Duration = Duration::from_secs(10);
const FIRE_COOLDOWN: Duration = Duration::from_secs(65);
/// 迟到唤醒补跑派发前的网络宽限（对齐 workbuddy2api #152）。
/// Windows Modern Standby exit 后 DNS/网络栈需 1–2s 恢复，零宽限派发会把
/// 唯一一次补跑机会打在注定失败的窗口。准点轮询不受影响。
const WAKEUP_GRACE_DELAY: Duration = Duration::from_secs(5);
/// 轮询间隔超过该阈值视为睡眠唤醒/迟到补跑（POLL_DELAY 的 3 倍）。
const WAKEUP_LATE_THRESHOLD: Duration = Duration::from_secs(90);
const TOKEN_KEEPALIVE_THRESHOLD_SECS: i64 = 2 * 60 * 60;
const NIGHT_WINDOW_HOURS: [u32; 9] = [23, 0, 1, 2, 3, 4, 5, 6, 7];

static STORAGE_LOCK: Mutex<()> = Mutex::new(());
static SCHEDULER_WAKE: OnceLock<Notify> = OnceLock::new();
static IS_RUNNING: Mutex<bool> = Mutex::new(false);
static LAST_POLL_AT: OnceLock<Mutex<Option<std::time::Instant>>> = OnceLock::new();

fn last_poll_at() -> &'static Mutex<Option<std::time::Instant>> {
    LAST_POLL_AT.get_or_init(|| Mutex::new(None))
}

/// 轮询间隔超阈值时视为迟到唤醒，先等网络/DNS 就绪再派发。
/// 返回是否继续执行（当前实现总是 true；预留取消语义）。
async fn await_wakeup_grace_if_late(reason: &str) {
    if reason == "手动立即触发" {
        return;
    }
    let late = match last_poll_at().lock() {
        Ok(guard) => match *guard {
            None => false,
            Some(prev) => prev.elapsed() > WAKEUP_LATE_THRESHOLD,
        },
        Err(_) => false,
    };
    if let Ok(mut guard) = last_poll_at().lock() {
        *guard = Some(std::time::Instant::now());
    }
    if late {
        logger::log_info(&format!(
            "[WorkbuddyScheduler] wakeup grace {:?}: late catch-up ({})",
            WAKEUP_GRACE_DELAY, reason
        ));
        tokio::time::sleep(WAKEUP_GRACE_DELAY).await;
    }
}

fn scheduler_wake() -> &'static Notify {
    SCHEDULER_WAKE.get_or_init(Notify::new)
}

fn wake_scheduler() {
    scheduler_wake().notify_one();
}

fn lock_storage() -> Result<MutexGuard<'static, ()>, String> {
    STORAGE_LOCK
        .lock()
        .map_err(|_| "WorkBuddy 活动调度存储锁已损坏".to_string())
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum ScheduleKind {
    Checkin,
    CatTravel,
    ActivityReport,
    SchoolSeason,
    NightCat,
    TokenKeepalive,
}

impl ScheduleKind {
    pub const ALL: [ScheduleKind; 6] = [
        ScheduleKind::Checkin,
        ScheduleKind::CatTravel,
        ScheduleKind::ActivityReport,
        ScheduleKind::SchoolSeason,
        ScheduleKind::NightCat,
        ScheduleKind::TokenKeepalive,
    ];

    pub fn as_str(self) -> &'static str {
        match self {
            Self::Checkin => "checkin",
            Self::CatTravel => "catTravel",
            Self::ActivityReport => "activityReport",
            Self::SchoolSeason => "schoolSeason",
            Self::NightCat => "nightCat",
            Self::TokenKeepalive => "tokenKeepalive",
        }
    }

    pub fn activity_log_kind(self) -> ActivityLogKind {
        match self {
            Self::Checkin => ActivityLogKind::Checkin,
            Self::CatTravel => ActivityLogKind::CatTravel,
            Self::ActivityReport => ActivityLogKind::ActivityReport,
            Self::SchoolSeason => ActivityLogKind::Growth,
            Self::NightCat => ActivityLogKind::NightCat,
            Self::TokenKeepalive => ActivityLogKind::TokenKeepalive,
        }
    }

    pub fn from_str(value: &str) -> Option<Self> {
        match value {
            "checkin" | "checkinEnabled" => Some(Self::Checkin),
            "catTravel" | "cat_travel" | "catTravelEnabled" => Some(Self::CatTravel),
            "activityReport" | "activity_report" | "activityReportEnabled" => {
                Some(Self::ActivityReport)
            }
            "schoolSeason" | "school_season" | "schoolSeasonEnabled" => Some(Self::SchoolSeason),
            "nightCat" | "night_cat" | "nightCatEnabled" => Some(Self::NightCat),
            "tokenKeepalive" | "token_keepalive" | "tokenKeepaliveEnabled" => {
                Some(Self::TokenKeepalive)
            }
            _ => None,
        }
    }

    pub fn label(self) -> &'static str {
        match self {
            Self::Checkin => "每日签到",
            Self::CatTravel => "猫猫旅行",
            Self::ActivityReport => "活跃上报",
            Self::SchoolSeason => "开学季任务",
            Self::NightCat => "夜猫子任务",
            Self::TokenKeepalive => "Token 保活",
        }
    }

    /// 官方默认触发小时（CST）。
    pub fn default_hours(self) -> &'static [u32] {
        match self {
            Self::Checkin | Self::CatTravel => &[9, 21],
            Self::ActivityReport => &[10],
            Self::SchoolSeason => &[12],
            Self::NightCat => &[1],
            Self::TokenKeepalive => &[22],
        }
    }

    /// 是否仅国内版账号适用。
    pub fn cn_only(self) -> bool {
        !matches!(self, Self::TokenKeepalive)
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkbuddyActivityScheduleConfig {
    pub enabled: bool,
    pub checkin_enabled: bool,
    pub cat_travel_enabled: bool,
    pub activity_report_enabled: bool,
    pub school_season_enabled: bool,
    pub night_cat_enabled: bool,
    pub token_keepalive_enabled: bool,
}

impl Default for WorkbuddyActivityScheduleConfig {
    fn default() -> Self {
        Self {
            enabled: true,
            checkin_enabled: true,
            cat_travel_enabled: true,
            activity_report_enabled: true,
            school_season_enabled: true,
            night_cat_enabled: true,
            token_keepalive_enabled: true,
        }
    }
}

impl WorkbuddyActivityScheduleConfig {
    pub fn is_kind_enabled(&self, kind: ScheduleKind) -> bool {
        if !self.enabled {
            return false;
        }
        match kind {
            ScheduleKind::Checkin => self.checkin_enabled,
            ScheduleKind::CatTravel => self.cat_travel_enabled,
            ScheduleKind::ActivityReport => self.activity_report_enabled,
            ScheduleKind::SchoolSeason => self.school_season_enabled,
            ScheduleKind::NightCat => self.night_cat_enabled,
            ScheduleKind::TokenKeepalive => self.token_keepalive_enabled,
        }
    }

    pub fn set_kind_enabled(&mut self, kind: ScheduleKind, enabled: bool) {
        match kind {
            ScheduleKind::Checkin => self.checkin_enabled = enabled,
            ScheduleKind::CatTravel => self.cat_travel_enabled = enabled,
            ScheduleKind::ActivityReport => self.activity_report_enabled = enabled,
            ScheduleKind::SchoolSeason => self.school_season_enabled = enabled,
            ScheduleKind::NightCat => self.night_cat_enabled = enabled,
            ScheduleKind::TokenKeepalive => self.token_keepalive_enabled = enabled,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ScheduleStore {
    #[serde(flatten)]
    config: WorkbuddyActivityScheduleConfig,
    #[serde(default)]
    last_fire_keys: HashMap<String, String>,
    #[serde(default)]
    last_run_at: Option<String>,
    #[serde(default)]
    last_summary: Option<String>,
}

impl Default for ScheduleStore {
    fn default() -> Self {
        Self {
            config: WorkbuddyActivityScheduleConfig::default(),
            last_fire_keys: HashMap::new(),
            last_run_at: None,
            last_summary: None,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkbuddyActivityScheduleStatus {
    pub enabled: bool,
    pub checkin_enabled: bool,
    pub cat_travel_enabled: bool,
    pub activity_report_enabled: bool,
    pub school_season_enabled: bool,
    pub night_cat_enabled: bool,
    pub token_keepalive_enabled: bool,
    pub last_run_at: Option<String>,
    pub last_summary: Option<String>,
}

fn store_path() -> PathBuf {
    config::get_shared_dir().join("workbuddy_activity_schedule.json")
}

fn read_store() -> Result<ScheduleStore, String> {
    let path = store_path();
    if !path.exists() {
        return Ok(ScheduleStore::default());
    }
    let content = fs::read_to_string(&path)
        .map_err(|e| format!("读取 WorkBuddy 活动调度配置失败: {}", e))?;
    crate::modules::atomic_write::parse_json_with_auto_restore(&path, &content)
        .map_err(|e| format!("解析 WorkBuddy 活动调度配置失败: {}", e))
}

fn write_store(store: &ScheduleStore) -> Result<(), String> {
    let path = store_path();
    let content = serde_json::to_string_pretty(store)
        .map_err(|e| format!("序列化 WorkBuddy 活动调度配置失败: {}", e))?;
    crate::modules::atomic_write::write_string_atomic(&path, &content)
        .map_err(|e| format!("保存 WorkBuddy 活动调度配置失败: {}", e))
}

pub fn get_schedule_status() -> Result<WorkbuddyActivityScheduleStatus, String> {
    let _guard = lock_storage()?;
    let store = read_store()?;
    Ok(WorkbuddyActivityScheduleStatus {
        enabled: store.config.enabled,
        checkin_enabled: store.config.checkin_enabled,
        cat_travel_enabled: store.config.cat_travel_enabled,
        activity_report_enabled: store.config.activity_report_enabled,
        school_season_enabled: store.config.school_season_enabled,
        night_cat_enabled: store.config.night_cat_enabled,
        token_keepalive_enabled: store.config.token_keepalive_enabled,
        last_run_at: store.last_run_at.clone(),
        last_summary: store.last_summary.clone(),
    })
}

pub fn get_logs() -> Result<Vec<activity_log::ActivityLogEntry>, String> {
    activity_log::get_activity_logs(None, None)
}

pub fn update_schedule(config: WorkbuddyActivityScheduleConfig) -> Result<(), String> {
    {
        let _guard = lock_storage()?;
        let mut store = read_store()?;
        store.config = config;
        write_store(&store)?;
    }
    wake_scheduler();
    Ok(())
}

fn fire_key(kind: ScheduleKind, now: &chrono::DateTime<Local>) -> String {
    format!("{}-{}", kind.as_str(), now.format("%Y-%m-%d-%H"))
}

fn is_cn_account(account: &WorkbuddyAccount) -> bool {
    workbuddy_activity::resolve_realm(account.domain.as_deref()) == WorkbuddyRealm::Cn
}

fn account_label(account: &WorkbuddyAccount) -> String {
    account
        .nickname
        .clone()
        .filter(|value| !value.trim().is_empty())
        .unwrap_or_else(|| account.email.clone())
}

fn already_checked_today(account: &WorkbuddyAccount) -> bool {
    let Some(ts) = account.last_checkin_time else {
        return false;
    };
    let now = Local::now();
    let Some(checkin) = Local.timestamp_opt(ts, 0).single() else {
        return false;
    };
    checkin.year() == now.year() && checkin.ordinal() == now.ordinal()
}

fn token_needs_keepalive(account: &WorkbuddyAccount) -> bool {
    let Some(expires_at) = account.expires_at else {
        return false;
    };
    if account.refresh_token.as_ref().map(|v| v.trim().is_empty()) != Some(false) {
        return false;
    }
    let now = Local::now().timestamp();
    expires_at.saturating_sub(now) < TOKEN_KEEPALIVE_THRESHOLD_SECS
}

fn night_window_active(now: &chrono::DateTime<Local>) -> bool {
    NIGHT_WINDOW_HOURS.contains(&now.hour())
}

async fn run_checkin_for_account(account: &WorkbuddyAccount) -> ActivityRunLog {
    let label = account_label(account);
    if !is_cn_account(account) {
        return ActivityRunLog {
            ok: false,
            account_id: account.id.clone(),
            label,
            earned_credit: 0,
            logs: vec!["国际版不适用国内签到".to_string()],
        };
    }
    if already_checked_today(account) {
        return ActivityRunLog {
            ok: true,
            account_id: account.id.clone(),
            label,
            earned_credit: 0,
            logs: vec!["今日已签到，跳过".to_string()],
        };
    }
    match codebuddy_cn_oauth::perform_checkin(
        &account.access_token,
        account.uid.as_deref(),
        account.enterprise_id.as_deref(),
        account.domain.as_deref(),
    )
    .await
    {
        Ok(response) => {
            if response.success {
                let now = Local::now().timestamp();
                let streak = response
                    .streak_days
                    .and_then(|value| i32::try_from(value).ok())
                    .unwrap_or_else(|| account.checkin_streak.unwrap_or(0).saturating_add(1));
                let reward = response.reward.clone().or_else(|| {
                    response
                        .credit
                        .map(|credit| serde_json::json!({ "credit": credit }))
                });
                let _ = workbuddy_account::update_checkin_info(
                    &account.id,
                    Some(now),
                    streak,
                    reward,
                );
                let credit = response.credit.unwrap_or(0);
                ActivityRunLog {
                    ok: true,
                    account_id: account.id.clone(),
                    label,
                    earned_credit: credit,
                    logs: vec![format!(
                        "✓ 签到成功: +{} 积分 (连登 {})",
                        credit,
                        response.streak_days.unwrap_or(0)
                    )],
                }
            } else {
                ActivityRunLog {
                    ok: false,
                    account_id: account.id.clone(),
                    label,
                    earned_credit: 0,
                    logs: vec![format!(
                        "签到未成功: {}",
                        response.message.unwrap_or_else(|| "未知原因".to_string())
                    )],
                }
            }
        }
        Err(error) => ActivityRunLog {
            ok: false,
            account_id: account.id.clone(),
            label,
            earned_credit: 0,
            logs: vec![format!("签到失败: {}", error)],
        },
    }
}

async fn execute_kind(kind: ScheduleKind) -> (bool, String, Vec<ActivityRunLog>) {
    let accounts = workbuddy_account::list_accounts();
    if accounts.is_empty() {
        return (true, "暂无 WorkBuddy 账号，跳过".to_string(), Vec::new());
    }
    let log_kind = kind.activity_log_kind();
    let mut run_logs: Vec<ActivityRunLog> = Vec::new();

    match kind {
        ScheduleKind::TokenKeepalive => {
            let mut refreshed = 0usize;
            let mut skipped = 0usize;
            let mut failed = 0usize;
            for account in &accounts {
                let label = account_label(account);
                if !token_needs_keepalive(account) {
                    skipped += 1;
                    continue;
                }
                match workbuddy_account::refresh_account_token(&account.id).await {
                    Ok(_) => {
                        refreshed += 1;
                        run_logs.push(ActivityRunLog {
                            ok: true,
                            account_id: account.id.clone(),
                            label,
                            earned_credit: 0,
                            logs: vec!["Token 已刷新".to_string()],
                        });
                    }
                    Err(err) => {
                        failed += 1;
                        logger::log_warn(&format!(
                            "[WorkbuddyScheduler] Token 保活失败 account={}: {}",
                            account.id, err
                        ));
                        run_logs.push(ActivityRunLog {
                            ok: false,
                            account_id: account.id.clone(),
                            label,
                            earned_credit: 0,
                            logs: vec![format!("Token 保活失败: {}", err)],
                        });
                    }
                }
                tokio::time::sleep(workbuddy_activity::MIN_REQUEST_GAP).await;
            }
            let ok = failed == 0;
            activity_log::append_from_run_logs(ActivityLogSource::Schedule, log_kind, &run_logs);
            (
                ok,
                format!(
                    "Token 保活: 刷新 {}，无需刷新 {}，失败 {}",
                    refreshed, skipped, failed
                ),
                run_logs,
            )
        }
        ScheduleKind::Checkin => {
            let mut success = 0usize;
            let mut skip = 0usize;
            let mut fail = 0usize;
            for account in &accounts {
                if !is_cn_account(account) {
                    skip += 1;
                    continue;
                }
                let result = run_checkin_for_account(account).await;
                if result.ok {
                    if result
                        .logs
                        .first()
                        .map(|m| m.contains("已签到"))
                        .unwrap_or(false)
                    {
                        skip += 1;
                    } else {
                        success += 1;
                    }
                } else {
                    fail += 1;
                }
                run_logs.push(result);
                tokio::time::sleep(workbuddy_activity::MIN_REQUEST_GAP).await;
            }
            activity_log::append_from_run_logs(ActivityLogSource::Schedule, log_kind, &run_logs);
            (
                fail == 0,
                format!("签到: 成功 {}，跳过 {}，失败 {}", success, skip, fail),
                run_logs,
            )
        }
        ScheduleKind::CatTravel | ScheduleKind::SchoolSeason | ScheduleKind::NightCat
        | ScheduleKind::ActivityReport => {
            let mut success = 0usize;
            let mut fail = 0usize;
            let mut earned = 0i64;
            for account in &accounts {
                if kind.cn_only() && !is_cn_account(account) {
                    continue;
                }
                let result = match kind {
                    ScheduleKind::CatTravel => {
                        match workbuddy_activity::cat_travel_for_account(&account.id).await {
                            Ok(log) => log,
                            Err(err) => ActivityRunLog {
                                ok: false,
                                account_id: account.id.clone(),
                                label: account_label(account),
                                earned_credit: 0,
                                logs: vec![err],
                            },
                        }
                    }
                    ScheduleKind::SchoolSeason => {
                        match workbuddy_activity::run_school_season_for_account(&account.id).await {
                            Ok(log) => log,
                            Err(err) => ActivityRunLog {
                                ok: false,
                                account_id: account.id.clone(),
                                label: account_label(account),
                                earned_credit: 0,
                                logs: vec![err],
                            },
                        }
                    }
                    ScheduleKind::NightCat => {
                        match workbuddy_activity::run_night_cat_for_account(&account.id).await {
                            Ok(log) => log,
                            Err(err) => ActivityRunLog {
                                ok: false,
                                account_id: account.id.clone(),
                                label: account_label(account),
                                earned_credit: 0,
                                logs: vec![err],
                            },
                        }
                    }
                    ScheduleKind::ActivityReport => {
                        match workbuddy_activity::run_activity_report_for_account(&account.id).await
                        {
                            Ok(log) => log,
                            Err(err) => ActivityRunLog {
                                ok: false,
                                account_id: account.id.clone(),
                                label: account_label(account),
                                earned_credit: 0,
                                logs: vec![err],
                            },
                        }
                    }
                    _ => unreachable!(),
                };
                if result.ok {
                    success += 1;
                } else {
                    fail += 1;
                }
                earned += result.earned_credit;
                run_logs.push(result);
                tokio::time::sleep(workbuddy_activity::MIN_REQUEST_GAP).await;
            }
            activity_log::append_from_run_logs(ActivityLogSource::Schedule, log_kind, &run_logs);
            (
                fail == 0,
                format!(
                    "{}: 成功账号 {}，失败 {}，到账 {} 积分",
                    kind.label(),
                    success,
                    fail,
                    earned
                ),
                run_logs,
            )
        }
    }
}

/// 执行一次调度周期。`force_kinds` 为空时按整点命中；非空时强制跑指定类。
pub async fn run_schedule_cycle(
    app: &AppHandle,
    reason: &str,
    force_kinds: Option<Vec<ScheduleKind>>,
) -> Result<(), String> {
    {
        let mut running = IS_RUNNING
            .lock()
            .map_err(|_| "WorkBuddy 活动调度运行锁已损坏".to_string())?;
        if *running {
            logger::log_info("[WorkbuddyScheduler] 已有调度周期在执行，跳过");
            return Ok(());
        }
        *running = true;
    }

    let result = run_schedule_cycle_inner(app, reason, force_kinds).await;

    if let Ok(mut running) = IS_RUNNING.lock() {
        *running = false;
    }
    result
}

async fn run_schedule_cycle_inner(
    app: &AppHandle,
    reason: &str,
    force_kinds: Option<Vec<ScheduleKind>>,
) -> Result<(), String> {
    // 迟到唤醒（睡眠跨过槽位）派发前网络宽限；手动触发跳过。
    if force_kinds.is_none() {
        await_wakeup_grace_if_late(reason).await;
    }
    let now = Local::now();
    let config = {
        let _guard = lock_storage()?;
        read_store()?.config
    };
    let is_force = force_kinds.is_some();

    // force_kinds：手动触发，忽略开关；否则按整点 + 开关筛选。
    let kinds: Vec<ScheduleKind> = match force_kinds {
        Some(list) => list,
        None => ScheduleKind::ALL
            .iter()
            .copied()
            .filter(|kind| {
                if !config.is_kind_enabled(*kind) {
                    return false;
                }
                if kind == &ScheduleKind::NightCat && !night_window_active(&now) {
                    return false;
                }
                kind.default_hours().contains(&now.hour())
            })
            .collect(),
    };

    if kinds.is_empty() {
        return Ok(());
    }

    logger::log_info(&format!(
        "[WorkbuddyScheduler] 开始执行 ({}) kinds={:?}",
        reason,
        kinds
            .iter()
            .map(|k| k.as_str())
            .collect::<Vec<_>>()
    ));

    for kind in kinds {
        let key = fire_key(kind, &now);
        let already = {
            let _guard = lock_storage()?;
            let store = read_store()?;
            store.last_fire_keys.get(kind.as_str()).map(|v| v == &key) == Some(true) && !is_force
        };
        if already {
            continue;
        }

        let (ok, message, _run_logs) = execute_kind(kind).await;
        {
            let _guard = lock_storage()?;
            let mut store = read_store()?;
            store.last_fire_keys.insert(kind.as_str().to_string(), key);
            store.last_run_at = Some(now.format("%Y-%m-%d %H:%M:%S").to_string());
            // 调度配置文件只保留汇总；账号级明细写 workbuddy_activity_logs.json
            store.last_summary = Some(format!(
                "[{}] {} {} — {}",
                now.format("%H:%M"),
                kind.label(),
                if ok { "OK" } else { "FAIL" },
                message
            ));
            write_store(&store)?;
        }
        let _ = app.emit(
            "workbuddy-activity-schedule-changed",
            serde_json::json!({ "kind": kind.as_str(), "ok": ok }),
        );
    }

    Ok(())
}

/// 手动立即执行指定类型。
pub async fn run_kind_now(app: &AppHandle, kind: ScheduleKind) -> Result<(), String> {
    run_schedule_cycle(app, "手动立即触发", Some(vec![kind])).await
}

pub fn start_workbuddy_activity_scheduler(app: AppHandle) {
    let wake = scheduler_wake();
    tauri::async_runtime::spawn(async move {
        logger::log_info("[WorkbuddyScheduler] 活动调度服务已启动");
        tokio::time::sleep(INITIAL_SWEEP_DELAY).await;
        if let Err(err) = run_schedule_cycle(&app, "启动初始化巡检", None).await {
            logger::log_warn(&format!("[WorkbuddyScheduler] 初始化巡检失败: {}", err));
        }
        // 初始化巡检后标记当前小时已触发，避免立刻再撞同一整点
        {
            if let Ok(_guard) = lock_storage() {
                if let Ok(mut store) = read_store() {
                    let now = Local::now();
                    for kind in ScheduleKind::ALL {
                        if store.config.is_kind_enabled(kind) {
                            store
                                .last_fire_keys
                                .insert(kind.as_str().to_string(), fire_key(kind, &now));
                        }
                    }
                    let _ = write_store(&store);
                }
            }
        }

        loop {
            let fired_recently = {
                if let Ok(_guard) = lock_storage() {
                    if let Ok(store) = read_store() {
                        store
                            .last_run_at
                            .as_ref()
                            .and_then(|value| {
                                chrono::NaiveDateTime::parse_from_str(value, "%Y-%m-%d %H:%M:%S")
                                    .ok()
                            })
                            .and_then(|ndt| Local.from_local_datetime(&ndt).single())
                            .map(|dt| {
                                Local::now().signed_duration_since(dt).to_std().unwrap_or_default()
                                    < FIRE_COOLDOWN
                            })
                            .unwrap_or(false)
                    } else {
                        false
                    }
                } else {
                    false
                }
            };
            if fired_recently {
                tokio::select! {
                    _ = tokio::time::sleep(FIRE_COOLDOWN) => {}
                    _ = wake.notified() => {}
                }
                continue;
            }

            match run_schedule_cycle(&app, "整点排程", None).await {
                Ok(_) => {
                    tokio::select! {
                        _ = tokio::time::sleep(POLL_DELAY) => {}
                        _ = wake.notified() => {}
                    }
                }
                Err(err) => {
                    logger::log_warn(&format!("[WorkbuddyScheduler] 调度异常: {}", err));
                    tokio::select! {
                        _ = tokio::time::sleep(POLL_DELAY) => {}
                        _ = wake.notified() => {}
                    }
                }
            }
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kind_from_str_accepts_camel_and_snake() {
        assert_eq!(ScheduleKind::from_str("checkin"), Some(ScheduleKind::Checkin));
        assert_eq!(
            ScheduleKind::from_str("cat_travel"),
            Some(ScheduleKind::CatTravel)
        );
        assert_eq!(
            ScheduleKind::from_str("tokenKeepalive"),
            Some(ScheduleKind::TokenKeepalive)
        );
        assert_eq!(ScheduleKind::from_str("nope"), None);
    }

    #[test]
    fn default_config_enables_all_kinds() {
        let config = WorkbuddyActivityScheduleConfig::default();
        assert!(config.enabled);
        for kind in ScheduleKind::ALL {
            assert!(config.is_kind_enabled(kind));
        }
    }

    #[test]
    fn night_window_contains_0100() {
        assert!(NIGHT_WINDOW_HOURS.contains(&1));
        assert!(NIGHT_WINDOW_HOURS.contains(&23));
        assert!(!NIGHT_WINDOW_HOURS.contains(&12));
    }
}
