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

任务写入 `DB_PATH` 指向的 SQLite 文件，重启后同一文件中的任务仍可查询。

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

### `POST /api/v1/jobs`

创建定时任务，任务 ID 由服务生成。请求体：

```json
{
  "name": "nightly report",
  "expression": "30 9 * * 1-5",
  "timezone": "Asia/Shanghai",
  "enabled": true
}
```

- `name`：必填、去空白后非空，且与未删除任务不重复。
- `expression`：五段 cron 表达式，依次为分钟、小时、月内日、月份、周内日；周内日取 `0`（周日）到 `6`（周六）。每段支持 `*`、数字、逗号列表（`1,2,3`）、范围（`1-5`）和步长（`*/15`、`0-30/5`）。
- `timezone`：IANA 时区名，例如 `UTC`、`America/New_York`、`Asia/Shanghai`。
- `enabled`：启用状态；省略时按 `true` 处理。

成功返回 HTTP 201：

```json
{
  "id": "3f1c...",
  "name": "nightly report",
  "expression": "30 9 * * 1-5",
  "timezone": "Asia/Shanghai",
  "enabled": true,
  "created_at": "2026-10-02T05:30:00Z",
  "next_run": "2026-10-02T01:30:00Z"
}
```

`next_run` 在任务所属时区计算、严格晚于请求处理时刻，统一以 UTC RFC3339 返回；任务停用时为 `null`。没有任何可到达触发时刻的表达式（例如 2 月 30 日）按 `schedule_invalid` 拒绝。

### `GET /api/v1/jobs/{id}`

返回单个任务，字段同上。任务不存在或已删除时返回 404 `job_not_found`。

### `PATCH /api/v1/jobs/{id}`

部分更新 `name`、`expression`、`timezone`、`enabled` 中出现的字段，返回 HTTP 200 与更新后的任务。修改表达式、时区或将任务重新启用时，按生效时刻重新计算 `next_run`；停用时置为 `null`，重新启用时以处理时刻为起点重算。仅修改名称等不影响调度的字段时 `next_run` 保持不变。

### `DELETE /api/v1/jobs/{id}`

删除任务（软删除），返回 HTTP 204 且无响应体。删除后该任务不可查询，其名称可被新任务复用。

### `GET /api/v1/jobs?state=pending&from=...&to=...`

按时间窗口查询待触发任务。

- `state`：当前仅支持 `pending`；省略时同样按待触发处理。
- `from`、`to`：必填的 RFC3339 时间戳，窗口左闭右开（`[from, to)`）。

返回启用、未删除且 `next_run` 落入窗口的任务，按 `next_run` 升序：

```json
[
  {"id": "...", "name": "hourly", "expression": "0 * * * *", "timezone": "UTC", "enabled": true, "created_at": "...", "next_run": "..."}
]
```

无匹配时返回 HTTP 200 与空数组 `[]`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。

| HTTP | code | 触发场景 |
|---|---|---|
| 400 | `invalid_json` | 请求体不是合法的单个 JSON 对象 |
| 400 | `time_window_required` | 窗口查询缺少 `from` 或 `to` |
| 400 | `time_window_invalid` | 时间戳格式错误、`from` 不早于 `to`，或 `state` 不支持 |
| 404 | `job_not_found` | 读取、修改或删除的任务不存在或已删除 |
| 404 | `route_not_found` | 请求未匹配任何路由 |
| 422 | `name_required` | 名称为空或仅空白 |
| 422 | `name_conflict` | 名称与其他未删除任务重复 |
| 422 | `schedule_invalid` | cron 表达式语法非法或不存在可到达的触发时刻 |
| 422 | `timezone_invalid` | 时区不是合法的 IANA 名称 |
| 503 | `storage_unavailable` | 存储不可用 |
