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

任务数据写入同一个 SQLite 文件；使用相同 `DB_PATH` 重启后，既有任务仍可查询。

## 定时表达式

表达式固定为五段，以空格分隔：`分钟 小时 月内日 月份 周内日`。

- 每段支持 `*`、数字、逗号列表（`1,2,3`）、范围（`1-5`）和步长（`*/15`、`0 9-17/2 * * *`）。
- 分钟 `0-59`，小时 `0-23`，月内日 `1-31`，月份 `1-12`，周内日 `0-6`（`0` 为周日，`7` 等价于 `0`）。
- 月内日与周内日至少一段受限时按标准 cron 语义取“或”：任一字段匹配即触发。
- 下次触发时间严格晚于处理时刻，先按任务的 IANA 时区计算，再以 UTC RFC3339 返回。

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

创建任务，任务 ID 由服务生成。请求体：

```json
{"name":"nightly","expression":"0 2 * * *","timezone":"Asia/Shanghai","enabled":true}
```

成功返回 HTTP 201 与完整任务：

```json
{
  "id": "8b2f0d11-0f0c-4f5e-bc8d-2c6a1d7e9a01",
  "name": "nightly",
  "expression": "0 2 * * *",
  "timezone": "Asia/Shanghai",
  "enabled": true,
  "created_at": "2026-10-02T05:38:16Z",
  "next_run": "2026-10-02T18:00:00Z"
}
```

`enabled` 为 `false` 时 `next_run` 为 `null`。

### `GET /api/v1/jobs?state=pending&from=...&to=...`

查询待触发任务。`from` 与 `to` 均为 UTC RFC3339 时间，窗口左闭右开；返回启用、未删除且 `next_run` 落窗的任务，按 `next_run` 升序排列。无匹配时 HTTP 200 返回 `[]`。

### `GET /api/v1/jobs/{id}`

返回单个任务；任务不存在返回 404 `job_not_found`。

### `PATCH /api/v1/jobs/{id}`

部分更新 `name`、`expression`、`timezone`、`enabled`，返回 HTTP 200 与更新后的任务。修改表达式、时区或重新启用时按当前生效时刻重算 `next_run`；停用时为 `null`。

### `DELETE /api/v1/jobs/{id}`

删除任务，成功返回 HTTP 204 且无响应体；任务不存在返回 404 `job_not_found`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。

| HTTP | `code` | 触发场景 |
|---|---|---|
| 400 | `invalid_json` | 请求体不是合法 JSON 或包含多个 JSON 值 |
| 400 | `time_window_required` | 窗口查询缺少 `from` 或 `to` |
| 400 | `time_window_invalid` | 窗口参数不是 RFC3339，或起始不早于结束 |
| 404 | `route_not_found` | 没有路由匹配请求路径与方法 |
| 404 | `job_not_found` | 任务 ID 不存在 |
| 422 | `name_required` | 创建或改名后的名称为空白 |
| 422 | `name_conflict` | 名称与其他任务重复 |
| 422 | `schedule_invalid` | cron 表达式非法或没有可触发日期 |
| 422 | `timezone_invalid` | 时区为空或不是合法 IANA 时区 |
| 503 | `storage_unavailable` | 存储不可用 |
