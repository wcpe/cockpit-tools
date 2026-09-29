//! CodeBuddy local-access request history + usage stats (SQLite).
//!
//! The sidecar keeps a live JSON ring buffer for governance; Cockpit persists
//! `codebuddy_usage` events into `codebuddy_local_access_logs.sqlite` so
//! history and aggregates survive service restarts and ring eviction.

use std::path::{Path, PathBuf};

use rusqlite::{params, Connection, Error as SqliteError};

use crate::models::codebuddy_local_access::{
    CodebuddyAccountModelStats, CodebuddyModelUsageRow, CodebuddyRequestLogPage,
    CodebuddyRequestRecord, CodebuddyUsageStats,
};
use crate::modules::{account, atomic_write, logger};

const LOGS_DB_FILE: &str = "codebuddy_local_access_logs.sqlite";
const BUSY_TIMEOUT_MS: u64 = 3_000;

fn logs_db_path() -> Result<PathBuf, String> {
    Ok(account::get_data_dir()?.join(LOGS_DB_FILE))
}

fn is_unusable_sqlite_error(error: &SqliteError) -> bool {
    let message = error.to_string().to_ascii_lowercase();
    message.contains("file is not a database")
        || message.contains("not a database")
        || message.contains("database disk image is malformed")
        || message.contains("database disk image is corrupt")
}

fn quarantine_logs_db(path: &Path, error: &SqliteError) -> Result<(), String> {
    let backup = atomic_write::quarantine_file(path, "invalid-sqlite")?;
    for suffix in ["-wal", "-shm"] {
        let sidecar = PathBuf::from(format!("{}{suffix}", path.display()));
        if sidecar.exists() {
            let _ = atomic_write::quarantine_file(&sidecar, "invalid-sqlite");
        }
    }
    logger::log_codex_api_warn(&format!(
        "[CodebuddyLocalAccess] 请求日志库异常已隔离: path={}, backup={}, error={}",
        path.display(),
        backup.map(|p| p.display().to_string()).unwrap_or_default(),
        error
    ));
    Ok(())
}

fn create_schema(conn: &Connection) -> Result<(), SqliteError> {
    conn.execute_batch(
        r#"
        PRAGMA journal_mode = WAL;
        PRAGMA synchronous = NORMAL;
        CREATE TABLE IF NOT EXISTS request_logs (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            event_key TEXT NOT NULL UNIQUE,
            request_id TEXT NOT NULL DEFAULT '',
            timestamp TEXT NOT NULL DEFAULT '',
            timestamp_unix_ms INTEGER NOT NULL DEFAULT 0,
            account_id TEXT NOT NULL DEFAULT '',
            account_label TEXT NOT NULL DEFAULT '',
            model TEXT NOT NULL DEFAULT '',
            client_stream INTEGER NOT NULL DEFAULT 0,
            outcome TEXT NOT NULL DEFAULT '',
            http_status INTEGER,
            latency_ms INTEGER NOT NULL DEFAULT 0,
            message TEXT NOT NULL DEFAULT '',
            reason_code TEXT NOT NULL DEFAULT '',
            reset_at TEXT NOT NULL DEFAULT '',
            conversation_request_id TEXT NOT NULL DEFAULT '',
            prompt_tokens INTEGER NOT NULL DEFAULT 0,
            completion_tokens INTEGER NOT NULL DEFAULT 0,
            total_tokens INTEGER NOT NULL DEFAULT 0,
            credit REAL NOT NULL DEFAULT 0,
            has_credit INTEGER NOT NULL DEFAULT 0,
            cached_tokens INTEGER NOT NULL DEFAULT 0,
            cache_write_tokens INTEGER NOT NULL DEFAULT 0,
            reasoning_tokens INTEGER NOT NULL DEFAULT 0,
            first_token_ms INTEGER NOT NULL DEFAULT 0,
            total_ms INTEGER NOT NULL DEFAULT 0,
            finish_reason TEXT NOT NULL DEFAULT '',
            attempt INTEGER NOT NULL DEFAULT 0,
            degraded_prompt INTEGER NOT NULL DEFAULT 0,
            prompt_mode TEXT NOT NULL DEFAULT '',
            include_reasoning INTEGER NOT NULL DEFAULT 0,
            message_count INTEGER NOT NULL DEFAULT 0,
            system_chars INTEGER NOT NULL DEFAULT 0,
            tool_count INTEGER NOT NULL DEFAULT 0,
            tool_choice TEXT NOT NULL DEFAULT '',
            max_tokens INTEGER,
            temperature REAL,
            top_p REAL,
            upstream_stream INTEGER NOT NULL DEFAULT 0
        );
        CREATE INDEX IF NOT EXISTS idx_cb_req_logs_ts
            ON request_logs(timestamp_unix_ms DESC);
        CREATE INDEX IF NOT EXISTS idx_cb_req_logs_model_ts
            ON request_logs(model, timestamp_unix_ms DESC);
        CREATE INDEX IF NOT EXISTS idx_cb_req_logs_account_ts
            ON request_logs(account_id, timestamp_unix_ms DESC);
        CREATE INDEX IF NOT EXISTS idx_cb_req_logs_outcome_ts
            ON request_logs(outcome, timestamp_unix_ms DESC);
        "#,
    )
}

fn open_logs_db() -> Result<(PathBuf, Connection), String> {
    let path = logs_db_path()?;
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)
            .map_err(|e| format!("创建日志目录失败: {}", e))?;
    }
    match open_logs_db_once(&path) {
        Ok(conn) => Ok((path, conn)),
        Err(error) if is_unusable_sqlite_error(&error) => {
            quarantine_logs_db(&path, &error)?;
            let conn = open_logs_db_once(&path)
                .map_err(|e| format!("重建请求日志库失败: {}", e))?;
            Ok((path, conn))
        }
        Err(error) => Err(format!("打开请求日志库失败: {}", error)),
    }
}

fn open_logs_db_once(path: &Path) -> Result<Connection, SqliteError> {
    let conn = Connection::open(path)?;
    conn.busy_timeout(std::time::Duration::from_millis(BUSY_TIMEOUT_MS))?;
    create_schema(&conn)?;
    Ok(conn)
}

fn opt_str(value: &Option<String>) -> String {
    value
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .unwrap_or_default()
        .to_string()
}

fn event_key(rec: &CodebuddyRequestRecord) -> String {
    format!(
        "{}|{}|{}|{}|{}|{}|{}",
        rec.id,
        rec.timestamp_unix_ms.unwrap_or_default(),
        opt_str(&rec.account_id),
        rec.model,
        rec.outcome,
        rec.attempt.unwrap_or_default(),
        opt_str(&rec.conversation_request_id),
    )
}

fn bool_to_i64(value: Option<bool>) -> i64 {
    if value.unwrap_or(false) {
        1
    } else {
        0
    }
}

fn insert_record(conn: &Connection, rec: &CodebuddyRequestRecord) -> Result<bool, SqliteError> {
    let key = event_key(rec);
    let changed = conn.execute(
        r#"
        INSERT OR IGNORE INTO request_logs (
            event_key, request_id, timestamp, timestamp_unix_ms,
            account_id, account_label, model, client_stream, outcome,
            http_status, latency_ms, message, reason_code, reset_at,
            conversation_request_id, prompt_tokens, completion_tokens,
            total_tokens, credit, has_credit, cached_tokens,
            cache_write_tokens, reasoning_tokens, first_token_ms, total_ms,
            finish_reason, attempt, degraded_prompt, prompt_mode,
            include_reasoning, message_count, system_chars, tool_count,
            tool_choice, max_tokens, temperature, top_p, upstream_stream
        ) VALUES (
            ?1, ?2, ?3, ?4,
            ?5, ?6, ?7, ?8, ?9,
            ?10, ?11, ?12, ?13, ?14,
            ?15, ?16, ?17,
            ?18, ?19, ?20, ?21,
            ?22, ?23, ?24, ?25,
            ?26, ?27, ?28, ?29,
            ?30, ?31, ?32, ?33,
            ?34, ?35, ?36, ?37, ?38
        )
        "#,
        params![
            key,
            rec.id,
            rec.timestamp,
            rec.timestamp_unix_ms.unwrap_or_default(),
            opt_str(&rec.account_id),
            opt_str(&rec.account_label),
            rec.model,
            bool_to_i64(Some(rec.client_stream)),
            rec.outcome,
            rec.http_status,
            rec.latency_ms.unwrap_or_default(),
            opt_str(&rec.message),
            opt_str(&rec.reason_code),
            opt_str(&rec.reset_at),
            opt_str(&rec.conversation_request_id),
            rec.prompt_tokens.unwrap_or_default(),
            rec.completion_tokens.unwrap_or_default(),
            rec.total_tokens.unwrap_or_default(),
            rec.credit.unwrap_or_default(),
            bool_to_i64(rec.has_credit),
            rec.cached_tokens.unwrap_or_default(),
            rec.cache_write_tokens.unwrap_or_default(),
            rec.reasoning_tokens.unwrap_or_default(),
            rec.first_token_ms.unwrap_or_default(),
            rec.total_ms.unwrap_or_default(),
            opt_str(&rec.finish_reason),
            rec.attempt.unwrap_or_default(),
            bool_to_i64(rec.degraded_prompt),
            opt_str(&rec.prompt_mode),
            bool_to_i64(rec.include_reasoning),
            rec.message_count.unwrap_or_default(),
            rec.system_chars.unwrap_or_default(),
            rec.tool_count.unwrap_or_default(),
            opt_str(&rec.tool_choice),
            rec.max_tokens,
            rec.temperature,
            rec.top_p,
            bool_to_i64(rec.upstream_stream),
        ],
    )?;
    Ok(changed > 0)
}

/// Persist one sidecar `codebuddy_usage` event (already parsed as a record).
pub fn record_usage_event(rec: &CodebuddyRequestRecord) -> Result<bool, String> {
    let (_path, conn) = open_logs_db()?;
    insert_record(&conn, rec).map_err(|e| format!("写入请求日志失败: {}", e))
}

/// Persist a batch (used by HTTP backfill from the live sidecar ring buffer).
pub fn record_usage_events(records: &[CodebuddyRequestRecord]) -> Result<u64, String> {
    if records.is_empty() {
        return Ok(0);
    }
    let (_path, conn) = open_logs_db()?;
    let mut inserted = 0u64;
    for rec in records {
        if insert_record(&conn, rec).map_err(|e| format!("批量写入请求日志失败: {}", e))? {
            inserted += 1;
        }
    }
    Ok(inserted)
}

fn read_str(row: &rusqlite::Row<'_>, name: &str) -> rusqlite::Result<String> {
    Ok(row.get::<_, Option<String>>(name)?.unwrap_or_default())
}

fn row_to_record(row: &rusqlite::Row<'_>) -> rusqlite::Result<CodebuddyRequestRecord> {
    let http_status: Option<i64> = row.get("http_status")?;
    let max_tokens: Option<i64> = row.get("max_tokens")?;
    let temperature: Option<f64> = row.get("temperature")?;
    let top_p: Option<f64> = row.get("top_p")?;
    let has_credit: i64 = row.get("has_credit")?;
    let degraded_prompt: i64 = row.get("degraded_prompt")?;
    let include_reasoning: i64 = row.get("include_reasoning")?;
    let upstream_stream: i64 = row.get("upstream_stream")?;
    let client_stream: i64 = row.get("client_stream")?;
    Ok(CodebuddyRequestRecord {
        id: read_str(row, "request_id")?,
        timestamp: read_str(row, "timestamp")?,
        timestamp_unix_ms: Some(row.get("timestamp_unix_ms")?),
        account_id: Some(read_str(row, "account_id")?)
            .filter(|s| !s.is_empty()),
        account_label: Some(read_str(row, "account_label")?)
            .filter(|s| !s.is_empty()),
        model: read_str(row, "model")?,
        client_stream: client_stream != 0,
        outcome: read_str(row, "outcome")?,
        http_status: http_status.map(|v| v as i32),
        latency_ms: Some(row.get::<_, i64>("latency_ms")?),
        message: Some(read_str(row, "message")?).filter(|s| !s.is_empty()),
        reason_code: Some(read_str(row, "reason_code")?).filter(|s| !s.is_empty()),
        reset_at: Some(read_str(row, "reset_at")?).filter(|s| !s.is_empty()),
        conversation_request_id: Some(read_str(row, "conversation_request_id")?)
            .filter(|s| !s.is_empty()),
        prompt_tokens: Some(row.get::<_, i64>("prompt_tokens")?),
        completion_tokens: Some(row.get::<_, i64>("completion_tokens")?),
        total_tokens: Some(row.get::<_, i64>("total_tokens")?),
        credit: Some(row.get::<_, f64>("credit")?),
        has_credit: Some(has_credit != 0),
        max_tokens,
        temperature,
        top_p,
        message_count: Some(row.get::<_, i64>("message_count")? as i32),
        system_chars: Some(row.get::<_, i64>("system_chars")? as i32),
        tool_count: Some(row.get::<_, i64>("tool_count")? as i32),
        tool_choice: Some(read_str(row, "tool_choice")?).filter(|s| !s.is_empty()),
        finish_reason: Some(read_str(row, "finish_reason")?).filter(|s| !s.is_empty()),
        attempt: Some(row.get::<_, i64>("attempt")? as i32),
        degraded_prompt: Some(degraded_prompt != 0),
        prompt_mode: Some(read_str(row, "prompt_mode")?).filter(|s| !s.is_empty()),
        include_reasoning: Some(include_reasoning != 0),
        cached_tokens: Some(row.get::<_, i64>("cached_tokens")?),
        cache_write_tokens: Some(row.get::<_, i64>("cache_write_tokens")?),
        reasoning_tokens: Some(row.get::<_, i64>("reasoning_tokens")?),
        upstream_stream: Some(upstream_stream != 0),
        first_token_ms: Some(row.get::<_, i64>("first_token_ms")?),
        total_ms: Some(row.get::<_, i64>("total_ms")?),
    })
}

fn count_all(conn: &Connection) -> Result<u64, String> {
    let total: i64 = conn
        .query_row("SELECT COUNT(*) FROM request_logs", [], |row| row.get(0))
        .map_err(|e| format!("统计请求日志失败: {}", e))?;
    Ok(total.max(0) as u64)
}

pub fn query_request_logs_page(offset: u32, limit: u32) -> Result<CodebuddyRequestLogPage, String> {
    let offset = offset.min(1_000_000);
    // Page size cap is only for one HTTP response; `total` is the full SQLite count
    // so the UI can page through every historical row (not the 500-row sidecar ring).
    let limit = if limit == 0 { 20 } else { limit.min(500) };
    let (_path, conn) = open_logs_db()?;
    let total = count_all(&conn)?;
    let mut stmt = conn
        .prepare(
            r#"
            SELECT request_id, timestamp, timestamp_unix_ms,
                   account_id, account_label, model, client_stream, outcome,
                   http_status, latency_ms, message, reason_code, reset_at,
                   conversation_request_id, prompt_tokens, completion_tokens,
                   total_tokens, credit, has_credit, cached_tokens,
                   cache_write_tokens, reasoning_tokens, first_token_ms, total_ms,
                   finish_reason, attempt, degraded_prompt, prompt_mode,
                   include_reasoning, message_count, system_chars, tool_count,
                   tool_choice, max_tokens, temperature, top_p, upstream_stream
            FROM request_logs
            ORDER BY timestamp_unix_ms DESC, id DESC
            LIMIT ?1 OFFSET ?2
            "#,
        )
        .map_err(|e| format!("准备查询失败: {}", e))?;
    let records = stmt
        .query_map(params![limit as i64, offset as i64], row_to_record)
        .map_err(|e| format!("查询请求日志失败: {}", e))?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| format!("读取请求日志失败: {}", e))?;
    let model_usage = query_model_usage_from_conn(&conn)?;
    Ok(CodebuddyRequestLogPage {
        records,
        total,
        offset,
        limit,
        model_usage: Some(model_usage),
    })
}

fn query_model_usage_from_conn(conn: &Connection) -> Result<Vec<CodebuddyModelUsageRow>, String> {
    let mut stmt = conn
        .prepare(
            r#"
            SELECT
                model,
                COUNT(*) AS requests,
                SUM(
                    CASE
                        WHEN cached_tokens > 0 AND cached_tokens <= prompt_tokens
                            THEN MAX(prompt_tokens - cached_tokens, cache_write_tokens)
                        ELSE prompt_tokens
                    END
                ) AS input_new,
                SUM(cached_tokens) AS cache_read,
                SUM(
                    CASE
                        WHEN cached_tokens > 0 AND cached_tokens <= prompt_tokens AND cache_write_tokens = 0
                            THEN MAX(prompt_tokens - cached_tokens, 0)
                        ELSE cache_write_tokens
                    END
                ) AS cache_write,
                SUM(completion_tokens) AS output_tokens,
                SUM(
                    CASE
                        WHEN total_tokens > 0 THEN total_tokens
                        WHEN cached_tokens > 0 AND cached_tokens <= prompt_tokens
                            THEN prompt_tokens + completion_tokens
                        ELSE prompt_tokens + cached_tokens + completion_tokens
                    END
                ) AS total_tokens,
                SUM(credit) AS credit,
                MAX(timestamp_unix_ms) AS last_at_ms,
                MAX(timestamp) AS last_at
            FROM request_logs
            WHERE outcome = 'ok' AND model != ''
            GROUP BY model
            ORDER BY last_at_ms DESC
            "#,
        )
        .map_err(|e| format!("准备用量聚合失败: {}", e))?;
    let rows = stmt
        .query_map([], |row| {
            let model: String = row.get("model")?;
            let requests: i64 = row.get("requests")?;
            let input_new: i64 = row.get("input_new")?;
            let cache_read: i64 = row.get("cache_read")?;
            let cache_write: i64 = row.get("cache_write")?;
            let output_tokens: i64 = row.get("output_tokens")?;
            let total_tokens: i64 = row.get("total_tokens")?;
            let credit: f64 = row.get("credit")?;
            let last_at: Option<String> = row.get("last_at")?;
            Ok(CodebuddyModelUsageRow {
                model,
                requests,
                input_tokens: input_new,
                cache_read,
                cache_write,
                total_input: input_new + cache_read,
                output_tokens,
                cache_hit_pct: {
                    let total_input = input_new + cache_read;
                    if total_input > 0 {
                        cache_read as f64 / total_input as f64 * 100.0
                    } else {
                        0.0
                    }
                },
                total_tokens,
                credit: Some(credit),
                last_at,
            })
        })
        .map_err(|e| format!("聚合用量失败: {}", e))?;
    rows.collect::<Result<Vec<_>, _>>()
        .map_err(|e| format!("读取用量失败: {}", e))
}

fn query_account_stats_from_conn(conn: &Connection) -> Result<Vec<CodebuddyAccountModelStats>, String> {
    let mut stmt = conn
        .prepare(
            r#"
            SELECT
                account_id,
                model,
                SUM(CASE WHEN outcome = 'ok' THEN 1 ELSE 0 END) AS ok,
                SUM(CASE WHEN outcome != 'ok' THEN 1 ELSE 0 END) AS fail,
                SUM(CASE WHEN outcome = 'ok' THEN credit ELSE 0 END) AS credit
            FROM request_logs
            WHERE account_id != '' AND model != ''
            GROUP BY account_id, model
            ORDER BY ok DESC, fail DESC
            "#,
        )
        .map_err(|e| format!("准备账号聚合失败: {}", e))?;
    let rows = stmt
        .query_map([], |row| {
            Ok(CodebuddyAccountModelStats {
                account_id: row.get("account_id")?,
                model: row.get("model")?,
                ok: row.get("ok")?,
                fail: row.get("fail")?,
                credit: row.get("credit")?,
            })
        })
        .map_err(|e| format!("聚合账号统计失败: {}", e))?;
    rows.collect::<Result<Vec<_>, _>>()
        .map_err(|e| format!("读取账号统计失败: {}", e))
}

pub fn query_usage_stats() -> Result<CodebuddyUsageStats, String> {
    let (_path, conn) = open_logs_db()?;
    let model_usage = query_model_usage_from_conn(&conn)?;
    let account_stats = query_account_stats_from_conn(&conn)?;
    let (total_requests, ok_requests, fail_requests, total_tokens, total_credit): (
        i64,
        i64,
        i64,
        i64,
        f64,
    ) = conn
        .query_row(
            r#"
            SELECT
                COUNT(*),
                SUM(CASE WHEN outcome = 'ok' THEN 1 ELSE 0 END),
                SUM(CASE WHEN outcome != 'ok' THEN 1 ELSE 0 END),
                SUM(CASE WHEN outcome = 'ok' THEN total_tokens ELSE 0 END),
                SUM(CASE WHEN outcome = 'ok' THEN credit ELSE 0 END)
            FROM request_logs
            "#,
            [],
            |row| {
                Ok((
                    row.get(0)?,
                    row.get::<_, Option<i64>>(1)?.unwrap_or_default(),
                    row.get::<_, Option<i64>>(2)?.unwrap_or_default(),
                    row.get::<_, Option<i64>>(3)?.unwrap_or_default(),
                    row.get::<_, Option<f64>>(4)?.unwrap_or_default(),
                ))
            },
        )
        .map_err(|e| format!("汇总用量失败: {}", e))?;
    Ok(CodebuddyUsageStats {
        model_usage,
        account_stats,
        total_requests: total_requests.max(0) as u64,
        ok_requests: ok_requests.max(0) as u64,
        fail_requests: fail_requests.max(0) as u64,
        total_tokens,
        total_credit,
    })
}

pub fn clear_request_logs() -> Result<(), String> {
    let (_path, conn) = open_logs_db()?;
    conn.execute("DELETE FROM request_logs", [])
        .map_err(|e| format!("清空请求日志失败: {}", e))?;
    Ok(())
}

pub fn request_logs_count() -> Result<u64, String> {
    let (_path, conn) = open_logs_db()?;
    count_all(&conn)
}

/// Backfill from live sidecar ring buffer when the durable store is empty.
pub fn backfill_from_sidecar_records(records: &[CodebuddyRequestRecord]) -> Result<u64, String> {
    if records.is_empty() {
        return Ok(0);
    }
    record_usage_events(records)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU64, Ordering};

    static TEMP_SEQ: AtomicU64 = AtomicU64::new(0);

    fn unique_temp_dir() -> PathBuf {
        let seq = TEMP_SEQ.fetch_add(1, Ordering::SeqCst);
        let dir = std::env::temp_dir().join(format!(
            "codebuddy-request-logs-{}-{}",
            std::process::id(),
            seq
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn sample_record(
        id: &str,
        model: &str,
        outcome: &str,
        prompt: i64,
        cache: i64,
    ) -> CodebuddyRequestRecord {
        CodebuddyRequestRecord {
            id: id.to_string(),
            timestamp: "2026-01-02T03:04:05Z".to_string(),
            timestamp_unix_ms: Some(1_767_323_045_000),
            account_id: Some("acct-1".to_string()),
            account_label: Some("WorkBuddy".to_string()),
            model: model.to_string(),
            client_stream: true,
            outcome: outcome.to_string(),
            http_status: Some(if outcome == "ok" { 200 } else { 429 }),
            latency_ms: Some(320),
            message: None,
            reason_code: if outcome == "ok" {
                None
            } else {
                Some("6004".to_string())
            },
            reset_at: None,
            conversation_request_id: Some("conv-1".to_string()),
            prompt_tokens: Some(prompt),
            completion_tokens: Some(10),
            total_tokens: Some(prompt + 10),
            credit: Some(0.05),
            has_credit: Some(true),
            max_tokens: None,
            temperature: None,
            top_p: None,
            message_count: Some(2),
            system_chars: Some(10),
            tool_count: None,
            tool_choice: None,
            finish_reason: None,
            attempt: Some(1),
            degraded_prompt: None,
            prompt_mode: None,
            include_reasoning: None,
            cached_tokens: Some(cache),
            cache_write_tokens: Some(0),
            reasoning_tokens: Some(0),
            upstream_stream: Some(true),
            first_token_ms: Some(80),
            total_ms: Some(300),
        }
    }

    fn open_temp_db(dir: &Path) -> Connection {
        let path = dir.join("test.sqlite");
        let conn = Connection::open(path).unwrap();
        conn.busy_timeout(std::time::Duration::from_millis(1000))
            .unwrap();
        create_schema(&conn).unwrap();
        conn
    }

    #[test]
    fn insert_and_query_model_usage() {
        let dir = unique_temp_dir();
        let conn = open_temp_db(&dir);
        assert!(insert_record(&conn, &sample_record("r1", "glm-5.1", "ok", 100, 90)).unwrap());
        assert!(insert_record(&conn, &sample_record("r2", "glm-5.1", "ok", 50, 0)).unwrap());
        assert!(
            insert_record(&conn, &sample_record("r3", "glm-5.1", "rate_limited", 0, 0)).unwrap()
        );
        assert!(!insert_record(&conn, &sample_record("r1", "glm-5.1", "ok", 100, 90)).unwrap());

        let usage = query_model_usage_from_conn(&conn).unwrap();
        assert_eq!(usage.len(), 1);
        let row = &usage[0];
        assert_eq!(row.model, "glm-5.1");
        assert_eq!(row.requests, 2);
        // r1: prompt includes cache 90 → input_new = 10, cache_read = 90
        // r2: no cache → input_new = 50, cache_read = 0
        assert_eq!(row.input_tokens, 60);
        assert_eq!(row.cache_read, 90);
        assert_eq!(row.output_tokens, 20);
        assert!(row.cache_hit_pct > 0.0);

        let stats = query_account_stats_from_conn(&conn).unwrap();
        assert_eq!(stats.len(), 1);
        assert_eq!(stats[0].ok, 2);
        assert_eq!(stats[0].fail, 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn page_orders_newest_first() {
        let dir = unique_temp_dir();
        let conn = open_temp_db(&dir);
        let mut older = sample_record("old", "hy3", "ok", 10, 0);
        older.timestamp_unix_ms = Some(1_000);
        let mut newer = sample_record("new", "hy3", "ok", 20, 0);
        newer.timestamp_unix_ms = Some(2_000);
        insert_record(&conn, &older).unwrap();
        insert_record(&conn, &newer).unwrap();
        let mut stmt = conn
            .prepare(
                "SELECT request_id FROM request_logs ORDER BY timestamp_unix_ms DESC LIMIT 1",
            )
            .unwrap();
        let id: String = stmt.query_row([], |row| row.get(0)).unwrap();
        assert_eq!(id, "new");
        let total: i64 = conn
            .query_row("SELECT COUNT(*) FROM request_logs", [], |row| row.get(0))
            .unwrap();
        assert_eq!(total, 2);
        let _ = std::fs::remove_dir_all(&dir);
    }
}
