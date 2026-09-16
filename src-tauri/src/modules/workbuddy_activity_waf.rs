//! WorkBuddy 活动中心 WAF 403 检测 + 账号级软退避。
//!
//! 对齐 workbuddy2api（`IsWafBlocked` / `ErrWafBlock` / WAF 403 软冷却）：
//! * **判定**：HTTP 403 且 body 无业务信封（不含 `"code":` 且不含 `"msg":`）——
//!   HTML 拦截页 / 空体 / 纯文本 / 非信封 JSON 均命中；带业务信封的 403（天数不足等）不命中。
//! * **Retry-After**：`Retry-After`（秒）/ `retry-after-ms` / `x-ratelimit-reset` 头族。
//! * **软冷却**：账号级进程内状态；基数 60s ±25% 抖动，连续触发 `base * 2^streak`
//!   封顶 30min；到期自动恢复；**不永久禁用**（WAF 是 IP/指纹维频控，账号本身健康）。
//! * **执行侧**：批量跑跳过冷却中账号；单号入口直接短路，避免继续撞风控。

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

/// WAF 软冷却基数（对齐 workbuddy2api `wafCooldownBase`）。
pub const WAF_COOLDOWN_BASE: Duration = Duration::from_secs(60);
/// 连续 WAF 冷却封顶（对齐 `soft_rate_max` 量级，活动中心取 30min 足够）。
pub const WAF_COOLDOWN_MAX: Duration = Duration::from_secs(30 * 60);

/// 报告 HTTP 403 响应是否为 WAF 拦截形态（无业务信封）。
///
/// 业务 403（如连登天数不足）body 含 `"code":` / `"msg":`，**不**判 WAF。
pub fn is_waf_blocked(status: u16, body: &str) -> bool {
    if status != 403 {
        return false;
    }
    !body.contains("\"code\":") && !body.contains("\"msg\":")
}

/// 从响应头解析上游明示的等待时长。
/// 支持：`Retry-After`（秒）、`retry-after-ms`（毫秒）、`x-ratelimit-reset`（unix 秒）。
pub fn parse_retry_after(headers: &reqwest::header::HeaderMap) -> Option<Duration> {
    if let Some(value) = headers
        .get("retry-after-ms")
        .or_else(|| headers.get("Retry-After-Ms"))
        .and_then(|v| v.to_str().ok())
    {
        if let Ok(ms) = value.trim().parse::<u64>() {
            if ms > 0 {
                return Some(Duration::from_millis(ms));
            }
        }
    }
    if let Some(value) = headers
        .get(reqwest::header::RETRY_AFTER)
        .and_then(|v| v.to_str().ok())
    {
        let value = value.trim();
        if let Ok(secs) = value.parse::<u64>() {
            if secs > 0 {
                return Some(Duration::from_secs(secs));
            }
        }
        // HTTP-date 形态不解析（活动中心短冷却场景收益极低）。
    }
    if let Some(value) = headers
        .get("x-ratelimit-reset")
        .or_else(|| headers.get("X-RateLimit-Reset"))
        .and_then(|v| v.to_str().ok())
    {
        if let Ok(unix_secs) = value.trim().parse::<u64>() {
            let now = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map(|d| d.as_secs())
                .unwrap_or(0);
            if unix_secs > now {
                return Some(Duration::from_secs(unix_secs - now));
            }
        }
    }
    None
}

#[derive(Debug, Clone)]
struct WafAccountState {
    /// 连续 WAF 次数（成功后清零）。
    streak: u32,
    /// 冷却截止时刻。
    until: Option<Instant>,
}

/// 账号级 WAF 软冷却闸（进程内单例）。
pub struct WafGate {
    states: Mutex<HashMap<String, WafAccountState>>,
}

impl WafGate {
    fn new() -> Self {
        Self {
            states: Mutex::new(HashMap::new()),
        }
    }

    /// 账号是否仍在 WAF 冷却中；返回剩余时长（None = 可执行）。
    pub fn remaining(&self, account_id: &str) -> Option<Duration> {
        let map = self.states.lock().ok()?;
        let state = map.get(account_id)?;
        let until = state.until?;
        let now = Instant::now();
        if until > now {
            Some(until - now)
        } else {
            None
        }
    }

    /// 是否冷却中（便捷布尔）。
    pub fn is_cooling(&self, account_id: &str) -> bool {
        self.remaining(account_id).is_some()
    }

    /// 记录一次 WAF 拦截：streak+1，冷却 `min(base * 2^(streak-1), max)`，
    /// 优先采信 `retry_after`。基数加 ±25% 抖动（防多账号同相位）。
    pub fn note_block(&self, account_id: &str, retry_after: Option<Duration>) {
        let Ok(mut map) = self.states.lock() else {
            return;
        };
        let state = map.entry(account_id.to_string()).or_insert(WafAccountState {
            streak: 0,
            until: None,
        });
        // 冷却中的兜底探测不翻倍（对齐 workbuddy2api CooldownSoftRate）。
        let cooling = state
            .until
            .map(|until| until > Instant::now())
            .unwrap_or(false);
        if !cooling {
            state.streak = state.streak.saturating_add(1);
        }
        let duration = match retry_after {
            Some(d) if d > Duration::ZERO => d.min(WAF_COOLDOWN_MAX),
            _ => {
                let shift = state.streak.saturating_sub(1).min(8);
                let base = WAF_COOLDOWN_BASE
                    .saturating_mul(1u32 << shift)
                    .min(WAF_COOLDOWN_MAX);
                jitter(base)
            }
        };
        state.until = Some(Instant::now() + duration);
    }

    /// 成功请求后清零 streak（不强制清冷却——冷却到期自然恢复）。
    pub fn note_ok(&self, account_id: &str) {
        if let Ok(mut map) = self.states.lock() {
            if let Some(state) = map.get_mut(account_id) {
                state.streak = 0;
            }
        }
    }

    /// 测试/诊断：读 streak。
    #[cfg(test)]
    pub fn streak(&self, account_id: &str) -> u32 {
        self.states
            .lock()
            .ok()
            .and_then(|map| map.get(account_id).map(|s| s.streak))
            .unwrap_or(0)
    }
}

/// ±25% 抖动（基于微秒时间戳，无需外部 rng 依赖）。
fn jitter(base: Duration) -> Duration {
    let micros = base.as_micros() as u64;
    if micros == 0 {
        return base;
    }
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.subsec_nanos() as u64)
        .unwrap_or(0);
    // [0, 0.5) 映射到 [-25%, +25%]
    let frac = (nanos % 1000) as f64 / 1000.0; // 0..1
    let offset = (frac - 0.5) * 0.5; // -0.25..0.25
    let factor = 1.0 + offset;
    let adjusted = (micros as f64 * factor).round() as u64;
    Duration::from_micros(adjusted.max(1))
}

lazy_static::lazy_static! {
    static ref WAF_GATE: WafGate = WafGate::new();
}

/// 进程级 WAF 闸。
pub fn gate() -> &'static WafGate {
    &WAF_GATE
}

#[cfg(test)]
mod tests {
    use super::*;
    use reqwest::header::{HeaderMap, HeaderValue};

    #[test]
    fn waf_block_html_page() {
        assert!(is_waf_blocked(403, ""));
        assert!(is_waf_blocked(403, "<html><body>403 Forbidden</body></html>"));
        assert!(is_waf_blocked(403, "Forbidden"));
        assert!(is_waf_blocked(403, "{\"message\":\"blocked by waf\"}"));
    }

    #[test]
    fn business_403_not_waf() {
        assert!(!is_waf_blocked(403, "{\"code\":403,\"msg\":\"连续登录天数不足\"}"));
        assert!(!is_waf_blocked(403, r#"{"code": 0, "msg": "ok"}"#));
        assert!(!is_waf_blocked(400, ""));
        assert!(!is_waf_blocked(200, "<html>"));
    }

    #[test]
    fn parse_retry_after_seconds() {
        let mut headers = HeaderMap::new();
        headers.insert(
            reqwest::header::RETRY_AFTER,
            HeaderValue::from_static("120"),
        );
        assert_eq!(parse_retry_after(&headers), Some(Duration::from_secs(120)));
    }

    #[test]
    fn parse_retry_after_ms() {
        let mut headers = HeaderMap::new();
        headers.insert("retry-after-ms", HeaderValue::from_static("1500"));
        assert_eq!(parse_retry_after(&headers), Some(Duration::from_millis(1500)));
    }

    #[test]
    fn gate_cooldown_and_expiry() {
        let gate = WafGate::new();
        let id = "acc-waf-test";
        assert!(!gate.is_cooling(id));
        gate.note_block(id, Some(Duration::from_millis(50)));
        assert!(gate.is_cooling(id));
        assert_eq!(gate.streak(id), 1);
        std::thread::sleep(Duration::from_millis(80));
        assert!(!gate.is_cooling(id));
        gate.note_ok(id);
        assert_eq!(gate.streak(id), 0);
    }

    #[test]
    fn gate_exponential_without_probe_double() {
        let gate = WafGate::new();
        let id = "acc-waf-streak";
        gate.note_block(id, Some(Duration::from_secs(30)));
        assert_eq!(gate.streak(id), 1);
        // 冷却中再次 note_block（兜底探测）不翻倍
        gate.note_block(id, None);
        assert_eq!(gate.streak(id), 1);
    }
}
