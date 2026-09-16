//! WorkBuddy 活动中心本地缓存。
//!
//! 避免活动中心每次打开都对全部账号打一轮接口：
//! * **总览缓存**：每账号一份 `ActivityOverview`，默认 TTL 5 分钟。
//! * **执行结果缓存**：每账号 × 活动 kind 一份上次 `ActivityRunLog`，
//!   重开界面直接回填，不必重跑。
//!
//! 文件：`workbuddy_activity_overview_cache.json`。

use std::collections::HashMap;
use std::fs;
use std::path::PathBuf;
use std::sync::{Mutex, MutexGuard};

use chrono::{DateTime, Local, TimeZone};
use serde::{Deserialize, Serialize};

use crate::modules::{
    atomic_write, config,
    workbuddy_activity::{ActivityOverview, ActivityRunLog},
};

const DEFAULT_TTL_SECS: i64 = 300;
const MAX_CACHED_ACCOUNTS: usize = 200;
const MAX_CACHED_RUNS: usize = 500;

static STORAGE_LOCK: Mutex<()> = Mutex::new(());

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CachedOverview {
    pub overview: ActivityOverview,
    pub fetched_at: String,
    pub fetched_at_ts: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CachedRunLog {
    pub log: ActivityRunLog,
    pub kind: String,
    pub ran_at: String,
    pub ran_at_ts: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
struct OverviewCacheStore {
    #[serde(default)]
    entries: HashMap<String, CachedOverview>,
    /// key = `{account_id}::{kind}`
    #[serde(default)]
    runs: HashMap<String, CachedRunLog>,
}

fn run_key(account_id: &str, kind: &str) -> String {
    format!("{}::{}", account_id, kind)
}

fn cache_path() -> PathBuf {
    config::get_shared_dir().join("workbuddy_activity_overview_cache.json")
}

fn lock_storage() -> Result<MutexGuard<'static, ()>, String> {
    STORAGE_LOCK
        .lock()
        .map_err(|_| "WorkBuddy 活动缓存存储锁已损坏".to_string())
}

fn read_store() -> Result<OverviewCacheStore, String> {
    let path = cache_path();
    if !path.exists() {
        return Ok(OverviewCacheStore::default());
    }
    let content =
        fs::read_to_string(&path).map_err(|e| format!("读取 WorkBuddy 活动缓存失败: {}", e))?;
    atomic_write::parse_json_with_auto_restore(&path, &content)
        .map_err(|e| format!("解析 WorkBuddy 活动缓存失败: {}", e))
}

fn write_store(store: &OverviewCacheStore) -> Result<(), String> {
    let path = cache_path();
    let content = serde_json::to_string_pretty(store)
        .map_err(|e| format!("序列化 WorkBuddy 活动缓存失败: {}", e))?;
    atomic_write::write_string_atomic(&path, &content)
        .map_err(|e| format!("保存 WorkBuddy 活动缓存失败: {}", e))
}

fn is_fresh(entry: &CachedOverview, ttl_secs: i64) -> bool {
    let now = Local::now().timestamp();
    now.saturating_sub(entry.fetched_at_ts) < ttl_secs.max(0)
}

pub fn get_cached_overview(
    account_id: &str,
    ttl_secs: Option<i64>,
) -> Result<Option<ActivityOverview>, String> {
    let ttl = ttl_secs.unwrap_or(DEFAULT_TTL_SECS);
    let _guard = lock_storage()?;
    let store = read_store()?;
    Ok(store
        .entries
        .get(account_id)
        .filter(|entry| is_fresh(entry, ttl))
        .map(|entry| entry.overview.clone()))
}

/// 读缓存（忽略 TTL）。用于「先展示旧数据，后台再慢刷」。
pub fn get_cached_overview_stale(
    account_id: &str,
) -> Result<Option<(ActivityOverview, i64)>, String> {
    let _guard = lock_storage()?;
    let store = read_store()?;
    Ok(store
        .entries
        .get(account_id)
        .map(|entry| (entry.overview.clone(), entry.fetched_at_ts)))
}

pub fn put_overview(account_id: &str, overview: ActivityOverview) -> Result<(), String> {
    let _guard = lock_storage()?;
    let mut store = read_store()?;
    let now = Local::now();
    store.entries.insert(
        account_id.to_string(),
        CachedOverview {
            overview,
            fetched_at: now.format("%Y-%m-%d %H:%M:%S").to_string(),
            fetched_at_ts: now.timestamp(),
        },
    );
    if store.entries.len() > MAX_CACHED_ACCOUNTS {
        // 按抓取时间淘汰最旧
        let mut pairs: Vec<(String, i64)> = store
            .entries
            .iter()
            .map(|(k, v)| (k.clone(), v.fetched_at_ts))
            .collect();
        pairs.sort_by_key(|(_, ts)| *ts);
        let drop_count = store.entries.len() - MAX_CACHED_ACCOUNTS;
        for (key, _) in pairs.into_iter().take(drop_count) {
            store.entries.remove(&key);
        }
    }
    write_store(&store)
}

/// 记录某账号某活动的执行结果（重开界面回填用）。
pub fn put_run_log(account_id: &str, kind: &str, log: ActivityRunLog) -> Result<(), String> {
    let _guard = lock_storage()?;
    let mut store = read_store()?;
    let now = Local::now();
    store.runs.insert(
        run_key(account_id, kind),
        CachedRunLog {
            log,
            kind: kind.to_string(),
            ran_at: now.format("%Y-%m-%d %H:%M:%S").to_string(),
            ran_at_ts: now.timestamp(),
        },
    );
    if store.runs.len() > MAX_CACHED_RUNS {
        let mut pairs: Vec<(String, i64)> = store
            .runs
            .iter()
            .map(|(k, v)| (k.clone(), v.ran_at_ts))
            .collect();
        pairs.sort_by_key(|(_, ts)| *ts);
        let drop_count = store.runs.len() - MAX_CACHED_RUNS;
        for (key, _) in pairs.into_iter().take(drop_count) {
            store.runs.remove(&key);
        }
    }
    write_store(&store)
}

/// 读某账号某活动的上次执行结果。
pub fn get_run_log(account_id: &str, kind: &str) -> Result<Option<ActivityRunLog>, String> {
    let _guard = lock_storage()?;
    let store = read_store()?;
    Ok(store.runs.get(&run_key(account_id, kind)).map(|c| c.log.clone()))
}

/// 读全部上次执行结果（按 kind 过滤，None=全部）。
pub fn list_run_logs(kind: Option<&str>) -> Result<Vec<CachedRunLog>, String> {
    let _guard = lock_storage()?;
    let store = read_store()?;
    let mut out: Vec<CachedRunLog> = store
        .runs
        .values()
        .filter(|c| kind.map(|k| c.kind == k).unwrap_or(true))
        .cloned()
        .collect();
    out.sort_by_key(|c| std::cmp::Reverse(c.ran_at_ts));
    Ok(out)
}

pub fn invalidate_account(account_id: &str) -> Result<(), String> {
    let _guard = lock_storage()?;
    let mut store = read_store()?;
    let mut changed = store.entries.remove(account_id).is_some();
    let prefix = format!("{}::", account_id);
    let keys: Vec<String> = store
        .runs
        .keys()
        .filter(|k| k.starts_with(&prefix))
        .cloned()
        .collect();
    for key in keys {
        store.runs.remove(&key);
        changed = true;
    }
    if changed {
        write_store(&store)?;
    }
    Ok(())
}

/// 仅失效总览（保留执行结果日志回填）。
pub fn invalidate_overview_only(account_id: &str) -> Result<(), String> {
    let _guard = lock_storage()?;
    let mut store = read_store()?;
    if store.entries.remove(account_id).is_some() {
        write_store(&store)?;
    }
    Ok(())
}

pub fn clear_cache() -> Result<(), String> {
    let _guard = lock_storage()?;
    write_store(&OverviewCacheStore::default())
}

pub fn cache_snapshot() -> Result<CacheSnapshot, String> {
    let _guard = lock_storage()?;
    let store = read_store()?;
    let now = Local::now().timestamp();
    let mut newest: Option<DateTime<Local>> = None;
    for entry in store.entries.values() {
        if let Some(dt) = Local.timestamp_opt(entry.fetched_at_ts, 0).single() {
            newest = Some(match newest {
                Some(current) if current > dt => current,
                _ => dt,
            });
        }
    }
    Ok(CacheSnapshot {
        account_count: store.entries.len(),
        run_log_count: store.runs.len(),
        newest_fetched_at: newest.map(|dt| dt.format("%Y-%m-%d %H:%M:%S").to_string()),
        now_ts: now,
        default_ttl_secs: DEFAULT_TTL_SECS,
    })
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CacheSnapshot {
    pub account_count: usize,
    pub run_log_count: usize,
    pub newest_fetched_at: Option<String>,
    pub now_ts: i64,
    pub default_ttl_secs: i64,
}
