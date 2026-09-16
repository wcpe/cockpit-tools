// WorkBuddy 活动中心 Tauri 命令。

use crate::modules::workbuddy_activity::{self, ActivityOverview, ActivityRunLog};
use crate::modules::workbuddy_activity_log::{
    self as activity_log, ActivityLogEntry, ActivityLogKind, ActivityLogSource,
};
use crate::modules::workbuddy_scheduler::{
    self, ScheduleKind, WorkbuddyActivityScheduleConfig, WorkbuddyActivityScheduleStatus,
};

fn log_manual_run(kind: ActivityLogKind, result: &ActivityRunLog) {
    let kind_label = kind.label();
    activity_log::append_activity_log(
        ActivityLogSource::Manual,
        kind,
        result.account_id.clone(),
        result.label.clone(),
        result.ok,
        result.earned_credit,
        result
            .logs
            .first()
            .cloned()
            .unwrap_or_else(|| kind_label.to_string()),
        result.logs.clone(),
    );
}

#[tauri::command]
pub async fn workbuddy_activity_overview(
    account_id: String,
) -> Result<ActivityOverview, String> {
    workbuddy_activity::overview_for_account(&account_id).await
}

#[tauri::command]
pub async fn workbuddy_activity_run_growth(
    account_id: String,
) -> Result<ActivityRunLog, String> {
    let result = workbuddy_activity::run_growth_tasks_for_account(&account_id).await?;
    log_manual_run(ActivityLogKind::Growth, &result);
    Ok(result)
}

#[tauri::command]
pub async fn workbuddy_activity_cat_travel(
    account_id: String,
) -> Result<ActivityRunLog, String> {
    let result = workbuddy_activity::cat_travel_for_account(&account_id).await?;
    log_manual_run(ActivityLogKind::CatTravel, &result);
    Ok(result)
}

#[tauri::command]
pub async fn workbuddy_activity_run_checkin(
    account_id: String,
) -> Result<crate::modules::codebuddy_cn_oauth::CheckinResponse, String> {
    let account = crate::modules::workbuddy_account::load_account(&account_id)
        .ok_or_else(|| format!("账号不存在: {}", account_id))?;
    let label = account
        .nickname
        .clone()
        .filter(|v| !v.trim().is_empty())
        .unwrap_or_else(|| account.email.clone());
    let response = crate::modules::codebuddy_cn_oauth::perform_checkin(
        &account.access_token,
        account.uid.as_deref(),
        account.enterprise_id.as_deref(),
        account.domain.as_deref(),
    )
    .await?;
    if response.success {
        let now = chrono::Utc::now().timestamp();
        let streak = response
            .streak_days
            .and_then(|value| i32::try_from(value).ok())
            .unwrap_or_else(|| account.checkin_streak.unwrap_or(0).saturating_add(1));
        let reward = response.reward.clone().or_else(|| {
            response
                .credit
                .map(|credit| serde_json::json!({ "credit": credit }))
        });
        let _ = crate::modules::workbuddy_account::update_checkin_info(
            &account_id,
            Some(now),
            streak,
            reward,
        );
        activity_log::append_activity_log(
            ActivityLogSource::Manual,
            ActivityLogKind::Checkin,
            account_id.clone(),
            label,
            true,
            response.credit.unwrap_or(0),
            format!(
                "签到成功 (+{}, 连登 {})",
                response.credit.unwrap_or(0),
                response.streak_days.unwrap_or(0)
            ),
            vec![format!(
                "签到成功 · +{} 积分 · 连登 {} 天",
                response.credit.unwrap_or(0),
                response.streak_days.unwrap_or(0)
            )],
        );
    } else {
        activity_log::append_activity_log(
            ActivityLogSource::Manual,
            ActivityLogKind::Checkin,
            account_id.clone(),
            label,
            false,
            0,
            response
                .message
                .clone()
                .unwrap_or_else(|| "签到未成功".to_string()),
            vec![response
                .message
                .clone()
                .unwrap_or_else(|| "签到未成功".to_string())],
        );
    }
    Ok(response)
}

#[tauri::command]
pub async fn workbuddy_activity_run_activity_report(
    account_id: String,
) -> Result<ActivityRunLog, String> {
    let result = workbuddy_activity::run_activity_report_for_account(&account_id).await?;
    log_manual_run(ActivityLogKind::ActivityReport, &result);
    Ok(result)
}

#[tauri::command]
pub async fn workbuddy_activity_run_night_cat(
    account_id: String,
) -> Result<ActivityRunLog, String> {
    let result = workbuddy_activity::run_night_cat_for_account(&account_id).await?;
    log_manual_run(ActivityLogKind::NightCat, &result);
    Ok(result)
}

#[tauri::command]
pub async fn workbuddy_activity_run_task(
    account_id: String,
    task_code: String,
) -> Result<ActivityRunLog, String> {
    let result = workbuddy_activity::run_task_code_for_account(&account_id, &task_code).await?;
    log_manual_run(ActivityLogKind::Growth, &result);
    Ok(result)
}

/// 按 kind 执行单个账号（streakRewards / schoolSeason / miniprogram 等）。
#[tauri::command]
pub async fn workbuddy_activity_run_kind(
    kind: String,
    account_id: String,
) -> Result<ActivityRunLog, String> {
    let result = workbuddy_activity::run_kind_for_account(&kind, &account_id).await?;
    let log_kind = match kind.as_str() {
        "growth" | "schoolSeason" | "school" | "miniprogram" | "minichat" => {
            ActivityLogKind::Growth
        }
        "streakRewards" | "streak" | "activityReport" | "activity_report" => {
            ActivityLogKind::ActivityReport
        }
        "catTravel" | "cat" => ActivityLogKind::CatTravel,
        "nightCat" => ActivityLogKind::NightCat,
        "checkin" => ActivityLogKind::Checkin,
        _ => ActivityLogKind::Growth,
    };
    log_manual_run(log_kind, &result);
    let _ = crate::modules::workbuddy_activity_cache::invalidate_account(&result.account_id);
    Ok(result)
}

#[tauri::command]
pub fn workbuddy_activity_get_schedule() -> Result<WorkbuddyActivityScheduleStatus, String> {
    workbuddy_scheduler::get_schedule_status()
}

#[tauri::command]
pub fn workbuddy_activity_update_schedule(
    enabled: Option<bool>,
    checkin_enabled: Option<bool>,
    cat_travel_enabled: Option<bool>,
    activity_report_enabled: Option<bool>,
    school_season_enabled: Option<bool>,
    night_cat_enabled: Option<bool>,
    token_keepalive_enabled: Option<bool>,
) -> Result<WorkbuddyActivityScheduleStatus, String> {
    let mut config = workbuddy_scheduler::get_schedule_status().map(|status| {
        WorkbuddyActivityScheduleConfig {
            enabled: status.enabled,
            checkin_enabled: status.checkin_enabled,
            cat_travel_enabled: status.cat_travel_enabled,
            activity_report_enabled: status.activity_report_enabled,
            school_season_enabled: status.school_season_enabled,
            night_cat_enabled: status.night_cat_enabled,
            token_keepalive_enabled: status.token_keepalive_enabled,
        }
    })?;
    if let Some(value) = enabled {
        config.enabled = value;
    }
    if let Some(value) = checkin_enabled {
        config.checkin_enabled = value;
    }
    if let Some(value) = cat_travel_enabled {
        config.cat_travel_enabled = value;
    }
    if let Some(value) = activity_report_enabled {
        config.activity_report_enabled = value;
    }
    if let Some(value) = school_season_enabled {
        config.school_season_enabled = value;
    }
    if let Some(value) = night_cat_enabled {
        config.night_cat_enabled = value;
    }
    if let Some(value) = token_keepalive_enabled {
        config.token_keepalive_enabled = value;
    }
    workbuddy_scheduler::update_schedule(config)?;
    workbuddy_scheduler::get_schedule_status()
}

#[tauri::command]
pub async fn workbuddy_activity_run_schedule_now(
    app: tauri::AppHandle,
    kind: String,
) -> Result<(), String> {
    let kind = ScheduleKind::from_str(&kind)
        .ok_or_else(|| format!("未知调度类型: {}", kind))?;
    workbuddy_scheduler::run_kind_now(&app, kind).await
}

#[tauri::command]
pub fn workbuddy_activity_get_logs(
    kind: Option<String>,
    source: Option<String>,
) -> Result<Vec<ActivityLogEntry>, String> {
    activity_log::get_activity_logs(kind.as_deref(), source.as_deref())
}

#[tauri::command]
pub fn workbuddy_activity_clear_logs() -> Result<(), String> {
    activity_log::clear_activity_logs()
}

#[derive(serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ActivityOverviewItem {
    pub account_id: String,
    pub overview: ActivityOverview,
}

#[derive(serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ActivityOverviewAllResult {
    pub items: Vec<ActivityOverviewItem>,
    pub cache_account_count: usize,
    pub cache_newest_at: Option<String>,
}

/// 全账号总览（默认读缓存，`forceRefresh=true` 强制拉接口）。
#[tauri::command]
pub async fn workbuddy_activity_overview_all(
    force_refresh: Option<bool>,
) -> Result<ActivityOverviewAllResult, String> {
    let force = force_refresh.unwrap_or(false);
    let pairs = workbuddy_activity::overview_all_accounts(true, force).await?;
    // 执行后刷新缓存快照
    for (id, overview) in &pairs {
        let _ = crate::modules::workbuddy_activity_cache::put_overview(id, overview.clone());
    }
    let snapshot = crate::modules::workbuddy_activity_cache::cache_snapshot().unwrap_or_else(|_| {
        crate::modules::workbuddy_activity_cache::CacheSnapshot {
            account_count: 0,
            run_log_count: 0,
            newest_fetched_at: None,
            now_ts: 0,
            default_ttl_secs: 300,
        }
    });
    Ok(ActivityOverviewAllResult {
        items: pairs
            .into_iter()
            .map(|(account_id, overview)| ActivityOverviewItem {
                account_id,
                overview,
            })
            .collect(),
        cache_account_count: snapshot.account_count,
        cache_newest_at: snapshot.newest_fetched_at,
    })
}

/// 单账号强制刷新总览。
#[tauri::command]
pub async fn workbuddy_activity_refresh_account(
    account_id: String,
) -> Result<ActivityOverview, String> {
    workbuddy_activity::refresh_overview_account(&account_id).await
}

/// 全账号慢刷总览：账号间隔约 1.5s，通过事件回报进度。
#[tauri::command]
pub async fn workbuddy_activity_refresh_all_slow(
    app: tauri::AppHandle,
) -> Result<ActivityOverviewAllResult, String> {
    use tauri::Emitter;
    let gap = std::time::Duration::from_millis(1500);
    let pairs = workbuddy_activity::refresh_overview_all_slow(gap, |done, total, account_id| {
        let _ = app.emit(
            "workbuddy-activity-refresh-progress",
            serde_json::json!({ "done": done, "total": total, "accountId": account_id }),
        );
    })
    .await?;
    let snapshot = crate::modules::workbuddy_activity_cache::cache_snapshot().unwrap_or_else(|_| {
        crate::modules::workbuddy_activity_cache::CacheSnapshot {
            account_count: 0,
            run_log_count: 0,
            newest_fetched_at: None,
            now_ts: 0,
            default_ttl_secs: 300,
        }
    });
    Ok(ActivityOverviewAllResult {
        items: pairs
            .into_iter()
            .map(|(account_id, overview)| ActivityOverviewItem {
                account_id,
                overview,
            })
            .collect(),
        cache_account_count: snapshot.account_count,
        cache_newest_at: snapshot.newest_fetched_at,
    })
}

/// 读上次执行结果缓存（可选按 kind 过滤）。
#[tauri::command]
pub fn workbuddy_activity_get_cached_runs(
    kind: Option<String>,
) -> Result<Vec<crate::modules::workbuddy_activity_cache::CachedRunLog>, String> {
    crate::modules::workbuddy_activity_cache::list_run_logs(kind.as_deref())
}

/// 全账号批量执行某一类活动。
#[tauri::command]
pub async fn workbuddy_activity_run_kind_all(
    kind: String,
) -> Result<Vec<ActivityRunLog>, String> {
    let log_kind = match kind.as_str() {
        "growth" => ActivityLogKind::Growth,
        "schoolSeason" | "school" => ActivityLogKind::Growth,
        "miniprogram" | "minichat" => ActivityLogKind::Growth,
        "streakRewards" | "streak" => ActivityLogKind::ActivityReport,
        "catTravel" | "cat" => ActivityLogKind::CatTravel,
        "nightCat" => ActivityLogKind::NightCat,
        "activityReport" | "activity_report" => ActivityLogKind::ActivityReport,
        "checkin" => ActivityLogKind::Checkin,
        other => return Err(format!("未知活动类型: {}", other)),
    };
    let results = workbuddy_activity::run_kind_for_all_accounts(&kind).await?;
    activity_log::append_from_run_logs(ActivityLogSource::Manual, log_kind, &results);
    // 执行后失效对应账号缓存，下次打开强制刷新
    for item in &results {
        let _ = crate::modules::workbuddy_activity_cache::invalidate_account(&item.account_id);
    }
    Ok(results)
}

/// 一键日常：对全部国内版账号依次跑签到/猫猫/上报/成长（夜窗加夜猫）。
/// 日志在模块层逐条写入。
#[tauri::command]
pub async fn workbuddy_activity_run_daily_all() -> Result<Vec<ActivityRunLog>, String> {
    let results = workbuddy_activity::run_daily_all_accounts().await?;
    for item in &results {
        let _ = crate::modules::workbuddy_activity_cache::invalidate_account(&item.account_id);
    }
    Ok(results)
}

#[tauri::command]
pub fn workbuddy_activity_clear_overview_cache() -> Result<(), String> {
    crate::modules::workbuddy_activity_cache::clear_cache()
}
