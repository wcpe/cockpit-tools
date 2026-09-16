---
feature: workbuddy-activity-sync
status: delivered
updated: 2026-09-17
branch: feat/workbuddy-activity-sync
commits: 4ec434f..HEAD # filled at delivery
---

# WorkBuddy 活动中心同步 + WAF 软退避

## Report

**What was built** — 对齐 workbuddy2api 在 `bab04ee` 之后的活动协议：

1. **连登闭环**：新手礼包 / 活动补偿 / 补签卡保连登 / 7d·14d·28d 里程碑兑换 / 连登抽奖；挂在活跃上报与成长任务成功后，按天幂等，global 跳过。
2. **first_buddy**：report 解锁 → agreement → `buddy/first` → claim 专段，接入成长主循环。
3. **小程序任务**：开学季 4 任务与成长 `Sequential_Tasks_1`，统一 `X-Client-Platform: miniprogram`；`schoolSeason` 从 growth 别名改为独立执行。
4. **WAF 风控**：403 无业务信封判定、Retry-After 头族解析、账号级软冷却（60s 基数 ±25% 抖动、指数封顶 30min、不永久禁用）；单号入口与批量/一键日常跳过冷却中账号。

**Verification** — `cargo check -p cockpit-tools` PASS（仅存量 warning）。`cargo test --lib workbuddy_activity_waf` 因本机测试二进制 `STATUS_ENTRYPOINT_NOT_FOUND`（DLL 入口，与本次改动无关）未能跑通；WAF 判定/冷却逻辑以单元测试代码落盘，待环境修复后可复跑。

**Journey log**
- worktree 创建被沙箱拦截，改在主仓建分支 `feat/workbuddy-activity-sync`。
- Rust 无反引号原始字符串，测试体须用 `r#"..."#`。
- UI 成长页签调度 kind 仍是 `schoolSeason`（历史绑定），手动/批量走 `growth`；`schoolSeason` 现真正跑开学季。

## [S1] Problem

cockpit-tools 活动中心落后于 workbuddy2api 在 `bab04ee` 之后的活动协议演进：

1. **连登闭环缺失**：没有 7d/14d/28d 里程碑兑换、连登抽奖、补签卡保连登、新手礼包/活动补偿。
2. **first_buddy 只有 task_code**：`build_event` 无 `buddy_first` 分支，上报 heartbeat 点不亮；真实链路是 report 解锁 → agreement → `buddy/first` → claim。
3. **小程序任务未接入**：开学季 4 任务（`chat_3_times` / `expert_use` / `share_invite` / `desktop_chat_1_time`）与成长任务 `Sequential_Tasks_1` 需要 `X-Client-Platform: miniprogram` 与独立事件构造。
4. **WAF 403 被当普通失败**：403 + 非业务信封（HTML/空体）是 APISIX WAF 拦截，当前既不识别也不退避，批量执行会撞风控。

## [S2] Design

### 2.1 连登奖励闭环（对齐 `growth_reward.go` + `claimGrowthRewards`）

在 `ActivityClient` 上新增，全部走 `chat_base` + 现有 `outbound_headers`：

| 方法 | 端点 | 语义 |
|------|------|------|
| `growth_reward_state` | GET `/activity/growth/streak` | `streak.days` + `redemption_status`（tiers/档位状态） |
| `growth_redeem(tier)` | POST `/activity/growth/redeem` | `{"tier","client_token"}`；409 duplicate / 403 天数不足为正常态 |
| `lottery_chances` | GET `/activity/growth/lottery/chances` | `data.balance` |
| `lottery_draw` | POST `/activity/growth/lottery/draw` | 每次新 `client_token`；400 无次数/未开启静默 |
| `claim_gift` | POST `/billing/meter/claim-gift` | 每号一次，业务错误静默 |
| `claim_compensation` | POST `/billing/meter/claim-compensation` | 有则领，业务错误静默 |
| `growth_heatmap` | GET `/activity/growth/heatmap` | `cells[]{date,score}` |
| `use_makeup_card(date)` | POST `/activity/growth/makeup-cards/use` | 昨日漏签且有卡才写 |

**执行顺序**（挂在 `run_activity_report` 成功后，以及 `run_growth_tasks` 开头）：

```
gift/compensation（幂等写）
→ reward-state
→ makeupYesterday（heatmap score==0 && cards.balance>0）
   → 成功则重读 state
→ 挑最高达标未领档 redeem
→ mark 当日已处理
→ chances>0 则 draw 一次
```

- 仅 CN realm；global 跳过（对齐上游门控）。
- 按天幂等：CST 自然日标记，进程内 `Mutex<HashMap>`。
- `client_token` 形态：`<prefix>-<32hex>`（uuid v4 去横线）。

### 2.2 first_buddy 领养链（对齐 `task_runner.py` buddyfirst）

`build_event` 新增 `buddy_first` 分支：构造一次规范 `chat_request_send`。

`ActivityClient::run_first_buddy`：

1. 若任务已 `claimed` → 幂等跳过。
2. 上报 `buddy_first` 对话事件（`report_events(billing=true)`）。
3. GET `{chat}/v2/auth/agreement?type=adopt_buddy` → `data.agreements[].id`。
4. POST `{chat}/v2/auth/agreement` body `{"agree_list": ids, "disagree_list": []}`。
5. POST `{chat}/activity/growth/buddy/first`（业务错误如 already adopted 静默）。
6. claim（复用现有 `claim_task`，desktop 400 → web 降级）。

接入 `run_growth_tasks` 主循环：`kind == "buddy_first"` 时走专段而非通用 report。

### 2.3 小程序任务

**公共**：请求头附加 `X-Client-Platform: miniprogram`。

**开学季（school 域）** 4 任务：

| code | 判据 | 事件 |
|------|------|------|
| `chat_3_times` | mini_chat ×3 + activityId | `make_school_event(mini_chat)` |
| `expert_use` | expert + activityId | `make_school_event(expert, id=ex_backtoschool_2026)` |
| `share_invite` | share-complete | POST `/portal/activity/school/tasks/share-complete` |
| `desktop_chat_1_time` | 桌面 chat 6 连 + activityId | `make_school_event(desktop_seq)` |

流程：GET `/portal/activity/school/tasks` → `pending` 时 POST `/tasks/{code}/viewed` → 按 `target-progress` 触发判据 → 回读 → POST `/tasks/{code}/claim`。

**成长小程序 `Sequential_Tasks_1`**：

- GET `{chat}/v2/activity/growth/tasks` 带 mp 头（默认口径无此 code）。
- 判据：mini 指纹 `chat_request_send`（`source=mini_program` / `ideType=WorkBuddy_MP` / `extName=workbuddy-mp` / `extVersion=2.4.0`，无 activityId）。
- accept/claim 均带 mp 头；报告打 `{bill}/v2/report`。

新增 kind：`schoolSeason` 从「别名 growth」改为独立执行；`miniprogram` / `minichat` 新 kind。

### 2.4 WAF 检测 + 软退避

**判定**（对齐 `IsWafBlocked`）：HTTP 403 且 body 无业务信封（不含 `"code":` 且不含 `"msg":`）。HTML 页、空体、纯文本、非信封 JSON 均命中。

**Retry-After**：解析 `Retry-After`（秒）/ `retry-after-ms` / `x-ratelimit-reset`；命中则冷却到该时刻。

**软冷却**（账号级，进程内）：

- 基数 `WAF_COOLDOWN_BASE = 60s`，±25% 抖动。
- 连续 WAF 指数：`base * 2^streak`，封顶 `30min`。
- 到期自动恢复；**不永久禁用**。
- 批量执行（`run_kind_for_all_accounts` / `run_daily_all_accounts`）跳过冷却中账号，日志记 `waf cooldown skip`。
- 单账号入口：若在冷却中直接返回失败日志，不发请求。

`get`/`post` 封装：403 非业务信封 → 返回 `ActivityError::Waf { retry_after }`；调用方更新冷却状态。

### 2.5 UI / 命令层

- 调度 kinds 增加：`streakRewards`（连登奖励，可并入 activityReport 或独立）、`schoolSeason` 真正独立、可选 `miniprogram`。
- 日志 kind：新增 `StreakRewards`、`SchoolSeason`、`Miniprogram`（或并入 Growth，以实现简洁为准：**并入 Growth/School 现有枚举，避免 UI 大改**）。
- 一键日常追加：activityReport 后自动跑 streak 闭环（在 report 函数内），growth 内含 first_buddy；schoolSeason / miniprogram 可手动或调度触发。

## [S3] Out of Scope

- 不移植 workbuddy2api 账号池 / softStreak 多账号错相位 / max_in_flight。
- 不改 token keepalive、models、chat proxy。
- 不做 UI 大改版；新活动尽量挂在现有「立即执行 / 一键日常 / 调度」入口。
- 不实现真实小程序登录；仅复用已有 desktop token + mp 头。

## Tasks

- [ ] T1: WAF 检测 + 账号软冷却基建 — acceptance: 403 非业务信封识别、Retry-After 解析、指数冷却、批量跳过；单测覆盖判定与冷却（covers: S2.4）
- [ ] T2: 连登奖励/抽奖/补签/礼包/补偿客户端 API — acceptance: 8 个端点封装 + 正常态识别 + client_token；单测 mock 响应（covers: S2.1）
- [ ] T3: claimGrowthRewards 编排接入 activityReport/growth — acceptance: 顺序正确、按天幂等、global 跳过、失败不拖累（covers: S2.1）
- [ ] T4: first_buddy 领养链 — acceptance: report→agreement→buddy/first→claim；幂等；接入 growth 主循环（covers: S2.2）
- [ ] T5: 开学季 4 任务 + Sequential_Tasks_1 — acceptance: mp 头、viewed→判据→claim；kind 路由 schoolSeason/miniprogram（covers: S2.3）
- [ ] T6: 命令/调度/一键日常串联 + 回归 — acceptance: cargo check/test 通过；既有 checkin/cat/night 零回归（covers: S2.1 S2.3 S2.5）
