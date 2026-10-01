# cron-job-manager

把定时任务的调度表达式、计划/实际/下次触发时间、执行结果（含失败原因）和失败重试记录成可查询的服务，支持按时间窗口查询待触发与已执行任务。

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

## 时间口径

- 所有请求与响应中的时间均为 RFC3339（`2006-01-02T15:04:05Z` 或带偏移量，如 `2026-10-01T18:00:00+08:00`）。
- 输入接受任意时区偏移，按绝对时刻比较；响应统一规范化为 UTC，带显式 `Z` 后缀。
- 计算与比较不依赖运行机器的本地时区，服务换时区后对同一输入给出同一响应。

## 时间字段语义（各入口一致）

- `scheduled_for`：本次（或某次重试）的计划触发时间。
- `triggered_at`：实际触发时间；`started_at` / `finished_at`：执行开始/结束时间。
- `next_scheduled_for`：本次执行完成或读取任务时，调度表达式推导出的下一次计划触发时间。
- 任务对象（创建/更新/查询任务）返回 `cron_expression` 与 `next_scheduled_for`；执行记录额外携带 `scheduled_for`、`triggered_at`、`started_at`、`finished_at`。
- `record_type` 稳定标识记录类型：`original`（原始执行）、`retry`（重试执行）、`pending`（窗口内尚未执行的计划触发）。

## 调度表达式

标准五段 cron：分 时 日 月 周，例如 `0 9 * * 1-5`（工作日 09:00）、`*/15 * * * *`（每 15 分钟）、`0 0 1 jan,jul *`。支持 `*`、单值、逗号列表、`a-b` 区间、`*/n` 与 `a-b/n` 步长，月/周支持 `JAN-DEC` / `SUN-SAT`，周字段 `0` 和 `7` 均表示周日。表达式非法或在可预见范围内永不触发（如 2 月 31 日）时，创建/更新返回 `invalid_cron_expression`。

## 公开入口

所有路径位于 `/api/v1` 之下，任务 ID 为字符串，运行记录以自增 `seq`（正整数）标识。

### 任务管理

- `POST /jobs`：创建任务。请求 `{"name":"nightly","cron_expression":"0 2 * * *"}`，返回 201 与任务当前调度信息。
- `GET /jobs/{jobID}`：返回任务与其当前的 `next_scheduled_for`。
- `PUT /jobs/{jobID}`：更新名称与表达式，返回更新后的调度信息。
- `DELETE /jobs/{jobID}`：删除任务（其运行记录一并删除），成功返回 204。

任务响应示例：

```json
{
  "id": "b1f0...",
  "name": "nightly",
  "cron_expression": "0 2 * * *",
  "next_scheduled_for": "2026-10-02T02:00:00Z",
  "created_at": "2026-10-01T09:00:00Z",
  "updated_at": "2026-10-01T09:00:00Z"
}
```

### 触发与手动执行

- `POST /jobs/{jobID}/runs`：调度触发执行入口（`trigger_type=scheduled`）。
- `POST /jobs/{jobID}/manual-runs`：手动执行入口（`trigger_type=manual`）。

请求体：

```json
{
  "scheduled_for": "2026-10-01T10:00:00Z",
  "triggered_at": "2026-10-01T10:00:02Z",
  "started_at": "2026-10-01T10:00:02Z",
  "finished_at": "2026-10-01T10:00:05Z",
  "result": "failure",
  "failure_reason": "upstream timeout"
}
```

- `result` 必填，只允许 `success` 或 `failure`。
- `failure` 必须携带非空 `failure_reason`；`success` 不允许携带 `failure_reason`，响应中该字段为 `null`。
- `scheduled_for` 缺省取当前时刻；`triggered_at` 缺省不早于当前与计划时刻；`started_at`、`finished_at` 依次顺延。
- 对同一计划时刻重复触发会生成相互独立的原始记录，不会合并或覆盖。
- 成功返回 201，记录立即可被窗口查询检索到。

### 重试

`POST /runs/{seq}/retries`：对一条**失败的原始执行**发起重试。请求体同上（`scheduled_for` 表示本次重试的计划时间）。行为：

- 新记录 `record_type=retry`、`retry_of_seq` 指向原始记录、`retry_number` 从 1 起递增。
- 原始记录的 `retry_count` 更新为当前重试次数；重试记录本身 `retry_count=0`。
- 重试只允许针对失败的原始记录；成功记录或重试行再重试返回 `retry_not_allowed`；`seq` 不存在返回 `run_not_found`。

### 时间窗口查询

`GET /run-records?start={RFC3339}&end={RFC3339}`

- `start`、`end` 均必填，按 `scheduled_for` 做**闭区间**筛选。
- 分别返回 `pending`（窗口内尚无原始执行记录的计划触发）与 `executed`（已产生结果的原始执行与全部重试）。
- `executed` 按 `scheduled_for`、再按写入顺序排列，保持同一任务多次执行（含重试）的时间顺序；`pending` 按 `scheduled_for` 升序。
- 无匹配数据时返回 200 与空数组（`[]`，不是 `null`）。
- `start` 晚于 `end` 返回唯一错误 `invalid_time_window`；跨度超过 366 天返回唯一错误 `query_range_too_large`。

响应示例：

```json
{
  "window_start": "2026-10-01T09:00:00Z",
  "window_end": "2026-10-01T12:00:00Z",
  "pending": [
    {"job_id":"...","job_name":"hourly","cron_expression":"0 * * * *","record_type":"pending","scheduled_for":"2026-10-01T11:00:00Z"}
  ],
  "executed": [
    {"seq":1,"job_id":"...","job_name":"hourly","cron_expression":"0 * * * *",
     "record_type":"original","trigger_type":"scheduled","retry_number":0,"retry_count":1,
     "scheduled_for":"2026-10-01T10:00:00Z","triggered_at":"2026-10-01T10:00:00Z",
     "started_at":"2026-10-01T10:00:00Z","finished_at":"2026-10-01T10:00:00Z",
     "result":"failure","failure_reason":"boom","next_scheduled_for":"2026-10-01T11:00:00Z"},
    {"seq":2,"job_id":"...","job_name":"hourly","cron_expression":"0 * * * *",
     "record_type":"retry","trigger_type":"scheduled","retry_of_seq":1,"retry_number":1,
     "retry_count":0,"scheduled_for":"2026-10-01T10:05:00Z","triggered_at":"2026-10-01T10:05:00Z",
     "started_at":"2026-10-01T10:05:00Z","finished_at":"2026-10-01T10:05:00Z",
     "result":"success","failure_reason":null,"next_scheduled_for":"2026-10-01T11:00:00Z"}
  ]
}
```

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。

| HTTP | code | 触发条件 |
|---|---|---|
| 400 | `invalid_request_body` | 请求体缺失或不是合法的单个 JSON 对象 |
| 400 | `invalid_job_name` | 任务名称为空或超过 256 字符 |
| 400 | `invalid_cron_expression` | 调度表达式非法或永不触发 |
| 400 | `invalid_run_result` | `result` 缺失或不是 `success`/`failure` |
| 400 | `invalid_failure_reason` | 失败缺少原因、成功携带原因，或原因超长 |
| 400 | `invalid_time` | 时间字段不是 RFC3339，或不满足 plan ≤ trigger ≤ start ≤ finish |
| 400 | `invalid_time_window` | 窗口缺少参数，或 `start` 晚于 `end`（唯一时间窗口非法异常） |
| 400 | `query_range_too_large` | 窗口跨度超过 366 天（唯一范围过大异常） |
| 404 | `job_not_found` | 任务不存在（唯一任务不存在异常） |
| 404 | `run_not_found` | 运行/重试记录不存在，或 `seq` 不是正整数 |
| 409 | `retry_not_allowed` | 对成功记录或非原始记录发起重试 |
| 404 | `route_not_found` | 路径不匹配 |
| 503 | `storage_unavailable` | 健康检查发现存储不可用 |
| 500 | `internal_error` | 未预期的服务端错误（不泄露内部细节） |
