---
feature: workbuddy-activity-automation
status: delivered
updated: 2026-02-14
branch: main
commits: deacbe44..HEAD
---

# WorkBuddy 活动自动化（P1 续）

## Report

**What was built** — P1a 调度器 `workbuddy_scheduler.rs`（六类独立开关、整点去重、环形日志、持久化、启动巡检、国际版仅 token 保活）与 11 个 Tauri 命令；P1b 前端活动中心 Modal（成长/猫猫/夜猫/活跃上报/调度）与工具栏入口，zh-CN/en。修复真机 `derive_id` panic（MD5 hex 32 位误切 `[..36]`）。

**Verification** — `cargo check` / `cargo build` PASS；`tsc --noEmit` PASS；CDP 驱动（localhost:9333，不抢焦点）：`workbuddy_activity_run_night_cat` 对真实账号上报成功；`update_schedule` 开关持久化回读一致。

**Journey log** — Python `hexdigest()[:36]` 在 32 位串上等于取全串，Rust `[..36]` 会 panic；9222 可能被本机 Edge 占用，改用 9333；`Remove-Item` 进回收站需 `Clear-RecycleBin` 才释放空间。

## [S1] Problem

活动引擎 `workbuddy_activity.rs` 已就绪，但缺少调度器、Tauri 命令与前端入口，用户无法在界面上领取积分活动，也无法自动跑每日签到/成长任务/猫猫/夜猫子/活跃上报/token 保活。

## [S2] Design

### 六类独立任务（CST）

| Kind | 默认时刻 | 行为 |
|---|---|---|
| checkin | 09, 21 | 国内版账号每日签到；已签过跳过 |
| catTravel | 09, 21 | 猫猫旅行闭环（arrived 领奖 / idle 派出） |
| activityReport | 10 | 对话事件连发，点亮连登/对话任务 |
| schoolSeason | 12 | 成长任务：接取 → 上报 → claim（含顺手猫猫） |
| nightCat | 01 | 夜窗 23:00–08:00 内补 `black_cat` |
| tokenKeepalive | 22 | 剩余寿命 < 2h 时刷新 token |

### 调度器

- 文件：`src-tauri/src/modules/workbuddy_scheduler.rs`
- tokio 常驻；启动 10s 后初始化巡检；之后每 30s 轮询
- 命中整点用 `kind + YYYY-MM-DD-HH` 去重（比 `minute==0` 更稳），触发后 65s 内不重复
- 配置持久化：`<shared_dir>/workbuddy_activity_schedule.json`（`atomic_write`）
- 日志环形缓冲最近 60 条，并随配置落盘
- 国际版账号跳过签到/猫猫/成长/夜猫/活跃上报，只做 token 保活

### Tauri 命令

`commands/workbuddy_activity.rs`：
- `workbuddy_activity_overview(account_id)`
- `workbuddy_activity_run_growth(account_id)`
- `workbuddy_activity_cat_travel(account_id)`
- `workbuddy_activity_run_checkin(account_id)`
- `workbuddy_activity_run_activity_report(account_id)`
- `workbuddy_activity_run_night_cat(account_id)`
- `workbuddy_activity_get_schedule()` / `workbuddy_activity_update_schedule(...)`
- `workbuddy_activity_run_schedule_now(kind)` / `workbuddy_activity_get_logs()`

### 前端

- `types/workbuddyActivity.ts` + `services/workbuddyActivityService.ts`
- 各活动 Modal（对照 `CodebuddySuiteCheckinModal`）+ 调度配置弹窗（对照 `WorkbuddyAutoCheckinConfigModal`）
- WorkBuddy 账号页每个活动独立入口；文案 `t()`，补 zh-CN / en

## [S3] Out of Scope

- P2 账号池选号 / P3 流量治理 / P4 请求链路 / P5 辅助工具
- 开学季「抽奖」接口（引擎暂无；任务 claim 覆盖主路径）
- 与旧 `workbuddy_auto_checkin` 的合并（并存；新调度器可独立开关）

## Tasks

- [x] T1: 调度器 + Tauri 命令 + cargo check — acceptance: 命令注册成功，编译 0 error（covers: S2）
- [x] T2: 前端 types/service/Modal/入口/i18n + tsc — acceptance: tsc 0 error（covers: S2; depends: T1）
- [x] T3: 真机 CDP 验收 — acceptance: 手动领任务/派猫猫；调度开关持久化（covers: S2; depends: T2）
