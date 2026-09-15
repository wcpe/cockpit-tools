//! WorkBuddy 活动中心执行日志。
//!
//! 与调度配置、签到日志分离存储，避免多类日志串写。
//! 文件：`workbuddy_activity_logs.json`（环形缓冲，最多 200 条）。

use std::fs;
use std::path::PathBuf;
use std::sync::{Mutex, MutexGuard};

use chrono::Local;
use serde::{Deserialize, Serialize};

use crate::modules::{atomic_write, config, logger};

const MAX_LOG_ENTRIES: usize = 200;

static STORAGE_LOCK: Mutex<()> = Mutex::new(());

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub enum ActivityLogSource {
    Schedule,
    Manual,
}

impl ActivityLogSource {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Schedule => "schedule",
            Self::Manual => "manual",
        }
    }
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub enum ActivityLogKind {
    Checkin,
    Growth,
    CatTravel,
    NightCat,
    ActivityReport,
    TokenKeepalive,
}

impl ActivityLogKind {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Checkin => "checkin",
            Self::Growth => "growth",
            Self::CatTravel => "catTravel",
            Self::NightCat => "nightCat",
            Self::ActivityReport => "activityReport",
            Self::TokenKeepalive => "tokenKeepalive",
        }
    }

    pub fn label(self) -> &'static str {
        match self {
            Self::Checkin => "每日签到",
            Self::Growth => "成长任务",
            Self::CatTravel => "猫猫旅行",
            Self::NightCat => "夜猫子",
            Self::ActivityReport => "活跃上报",
            Self::TokenKeepalive => "Token 保活",
        }
    }

    pub fn from_str(value: &str) -> Option<Self> {
        match value {
            "checkin" => Some(Self::Checkin),
            "growth" | "schoolSeason" => Some(Self::Growth),
            "catTravel" | "cat" => Some(Self::CatTravel),
            "nightCat" => Some(Self::NightCat),
            "activityReport" | "activity_report" => Some(Self::ActivityReport),
            "tokenKeepalive" | "token_keepalive" => Some(Self::TokenKeepalive),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ActivityLogEntry {
    pub id: String,
    pub timestamp: String,
    pub source: String,
    pub kind: String,
    pub kind_label: String,
    pub account_id: String,
    pub account_label: String,
    pub ok: bool,
    #[serde(default)]
    pub earned_credit: i64,
    pub message: String,
    #[serde(default)]
    pub lines: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
struct ActivityLogStore {
    #[serde(default)]
    entries: Vec<ActivityLogEntry>,
}

fn logs_path() -> PathBuf {
    config::get_shared_dir().join("workbuddy_activity_logs.json")
}

fn lock_storage() -> Result<MutexGuard<'static, ()>, String> {
    STORAGE_LOCK
        .lock()
        .map_err(|_| "WorkBuddy 活动日志存储锁已损坏".to_string())
}

fn read_store() -> Result<ActivityLogStore, String> {
    let path = logs_path();
    if !path.exists() {
        return Ok(ActivityLogStore::default());
    }
    let content =
        fs::read_to_string(&path).map_err(|e| format!("读取 WorkBuddy 活动日志失败: {}", e))?;
    atomic_write::parse_json_with_auto_restore(&path, &content)
        .map_err(|e| format!("解析 WorkBuddy 活动日志失败: {}", e))
}

fn write_store(store: &ActivityLogStore) -> Result<(), String> {
    let path = logs_path();
    let content = serde_json::to_string_pretty(store)
        .map_err(|e| format!("序列化 WorkBuddy 活动日志失败: {}", e))?;
    atomic_write::write_string_atomic(&path, &content)
        .map_err(|e| format!("保存 WorkBuddy 活动日志失败: {}", e))
}

fn new_id() -> String {
    format!(
        "act_{}_{}",
        Local::now().timestamp_millis(),
        rand::random::<u16>()
    )
}

/// 追加一条活动执行日志（调度或手动）。
pub fn append_activity_log(
    source: ActivityLogSource,
    kind: ActivityLogKind,
    account_id: impl Into<String>,
    account_label: impl Into<String>,
    ok: bool,
    earned_credit: i64,
    message: impl Into<String>,
    lines: Vec<String>,
) {
    let entry = ActivityLogEntry {
        id: new_id(),
        timestamp: Local::now().format("%Y-%m-%d %H:%M:%S").to_string(),
        source: source.as_str().to_string(),
        kind: kind.as_str().to_string(),
        kind_label: kind.label().to_string(),
        account_id: account_id.into(),
        account_label: account_label.into(),
        ok,
        earned_credit,
        message: message.into(),
        lines,
    };
    logger::log_info(&format!(
        "[WorkbuddyActivityLog] {} {} account={} ok={} {}",
        entry.source, entry.kind_label, entry.account_label, entry.ok, entry.message
    ));
    if let Err(err) = (|| -> Result<(), String> {
        let _guard = lock_storage()?;
        let mut store = read_store()?;
        store.entries.insert(0, entry);
        if store.entries.len() > MAX_LOG_ENTRIES {
            store.entries.truncate(MAX_LOG_ENTRIES);
        }
        write_store(&store)
    })() {
        logger::log_warn(&format!("[WorkbuddyActivityLog] 写入失败: {}", err));
    }
}

/// 从 ActivityRunLog 批量落盘（每账号一条）。
pub fn append_from_run_logs(
    source: ActivityLogSource,
    kind: ActivityLogKind,
    logs: &[crate::modules::workbuddy_activity::ActivityRunLog],
) {
    for log in logs {
        append_activity_log(
            source,
            kind,
            log.account_id.clone(),
            log.label.clone(),
            log.ok,
            log.earned_credit,
            log.logs
                .first()
                .cloned()
                .unwrap_or_else(|| kind.label().to_string()),
            log.logs.clone(),
        );
    }
}

pub fn get_activity_logs(
    kind_filter: Option<&str>,
    source_filter: Option<&str>,
) -> Result<Vec<ActivityLogEntry>, String> {
    let _guard = lock_storage()?;
    let store = read_store()?;
    Ok(store
        .entries
        .into_iter()
        .filter(|entry| match kind_filter {
            None | Some("") => true,
            Some(kind) => entry.kind == kind,
        })
        .filter(|entry| match source_filter {
            None | Some("") => true,
            Some(source) => entry.source == source,
        })
        .collect())
}

pub fn clear_activity_logs() -> Result<(), String> {
    let _guard = lock_storage()?;
    write_store(&ActivityLogStore::default())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kind_from_str_aliases() {
        assert_eq!(ActivityLogKind::from_str("growth"), Some(ActivityLogKind::Growth));
        assert_eq!(
            ActivityLogKind::from_str("schoolSeason"),
            Some(ActivityLogKind::Growth)
        );
        assert_eq!(
            ActivityLogKind::from_str("nightCat"),
            Some(ActivityLogKind::NightCat)
        );
        assert_eq!(ActivityLogKind::from_str("nope"), None);
    }
}
