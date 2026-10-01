# cron-job-manager

把定时任务的调度表达式、下次触发时间、执行结果和失败重试记录成可查询的服务，支持按时间窗口查询待触发与已执行任务。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `cron-job-manager.db` | SQLite 数据库文件路径 |

## 时间与调度口径

- 调度使用标准五段 cron（`分 时 日 月 周`，支持 `*`、`,`、`-`、`/`、`@`-less 名称如 `mon`、`jan`），**按 UTC 解释、按分钟对齐**，与运行机器本地时区无关。
- 入参时间使用 RFC3339（必须带时区偏移，如 `2026-01-01T08:00:00+08:00` 或 `...Z`），返回时间一律带显式偏移（UTC，`...Z`），存库为 Unix 纳秒。
- 所有入口共用同一套时间字段语义：`planned_fire_time`（本次计划触发时间）、`actual_fire_time`（实际触发时间）、`next_fire_time`（相对本次计划时间的下次触发时间）、`started_at`/`finished_at`（执行开始/结束时间）。
- `record_type` 稳定标识记录类型：`original`（原始执行）、`retry`（重试执行）、`pending`（窗口内尚未执行的计划时刻）。
- 时间窗口为闭区间，按 `planned_fire_time` 筛选；最大允许跨度 90 天（`service.MaxQuerySpan`），刚好 90 天合法。

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### 任务管理

- `POST /api/v1/tasks`：创建任务。请求体 `{"id?","name","schedule","action"}`，省略 `id` 时自动生成；返回任务当前调度信息（含 `current_fire_time`、`next_fire_time`）。
- `PUT /api/v1/tasks/:id`：更新任务（名称/表达式/动作），返回同一套调度信息。
- `GET /api/v1/tasks/:id`、`GET /api/v1/tasks`：查询单个/全部任务及调度信息。
- `DELETE /api/v1/tasks/:id`：删除任务定义；历史运行记录保留可查。
- `POST /api/v1/tasks/schedules/validate`：校验调度表达式，`{"schedule":"..."}`。

内置动作（`action`）：`succeed` 恒成功；`fail` 以通用原因失败；`fail:<原因>` 以指定原因失败（失败原因可查询，成功记录不携带 `failure_reason`）。

### 触发、手动执行与重试

- `POST /api/v1/tasks/:id/trigger`：调度触发入口。请求体可省略；可给 `{"planned_fire_time":"..."}` 显式指定本次计划触发时间，省略时使用当前或最近的计划时刻。同一计划时刻重复触发各自形成**独立记录**，不合并、不覆盖。
- `POST /api/v1/tasks/:id/runs`：手动执行入口。计划触发时间即实际触发时刻，`trigger_type` 为 `manual`，仍是 `original` 记录。
- `POST /api/v1/runs/:runId/retry`：对**失败**记录重试，可给 `{"planned_fire_time":"..."}`（默认当前时刻）。新记录 `record_type=retry`，`attempt` 递增，携带 `original_run_id`/`parent_run_id` 与自身的计划时间、开始/结束时间、结果、失败原因。对成功/进行中记录重试返回 `run_not_retriable`。
- `GET /api/v1/runs/:runId`：查询单条运行记录。

触发或重试完成后记录立即落库、立即可查。

### 时间窗口查询

`GET /api/v1/runs?start=<RFC3339>&end=<RFC3339>`（闭区间，按计划触发时间）返回：

```json
{
  "window_start": "2026-01-01T10:00:00Z",
  "window_end":   "2026-01-01T10:30:00Z",
  "pending":  [ { "record_type": "pending", "attempt": 1, "task_id": "...", "task_name": "...",
                  "schedule": "*/15 * * * *", "trigger_type": "scheduled",
                  "planned_fire_time": "...", "next_fire_time": "...", "status": "pending" } ],
  "executed": [ { "record_type": "original|retry", "attempt": 1, "id": "...", "task_id": "...",
                  "task_name": "...", "schedule": "...", "trigger_type": "scheduled|manual",
                  "planned_fire_time": "...", "actual_fire_time": "...", "next_fire_time": "...",
                  "started_at": "...", "finished_at": "...", "status": "success|failure",
                  "failure_reason": "...(仅失败)", "original_run_id": "...(仅重试)",
                  "parent_run_id": "...(仅重试)" } ]
}
```

- `pending`：窗口内任务表达式枚举出来、且尚无原始调度执行记录的计划时刻；`executed`：已产生成功/失败结果的记录（原始与重试均在其中）。
- 列表按 `planned_fire_time` 升序，同一时刻按写入顺序排列，保持同一任务多次执行（含重试）的时间顺序。
- 无匹配数据时返回 HTTP 200 与空数组，不算异常。
- 历史记录保存当时的 `schedule` 与任务名快照，更新表达式不会改写旧记录。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。

唯一错误码：

| code | 状态码 | 含义 |
|---|---|---|
| `task_not_found` | 404 | 任务不存在 |
| `run_not_found` | 404 | 运行记录不存在 |
| `task_id_conflict` | 409 | 创建时任务 id 已存在 |
| `invalid_schedule` | 400 | 调度表达式非法或无法产生触发时刻 |
| `invalid_task` | 400 | 任务定义字段缺失 |
| `invalid_action` | 400 | 动作规格不受支持 |
| `run_not_retriable` | 409 | 仅失败记录可重试 |
| `invalid_time_window` | 400 | 起始晚于结束，或缺少 start/end |
| `query_range_too_large` | 400 | 窗口跨度超过 90 天 |
| `invalid_time_format` | 400 | 时间不是带偏移的 RFC3339 |
| `route_not_found` | 404 | 路径不存在 |
| `storage_unavailable` | 503 | 存储不可用（健康检查） |
| `internal_error` | 500 | 其余内部错误 |
