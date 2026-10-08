# 图片结果存储策略

`POST /v1/images/jobs` 支持以下可选参数。后台和门户的 jobs 接口也支持；同步 `/v1/images/generations`、`/v1/images/edits` 的协议不变。

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `storage_mode` | `legacy` | `legacy`、`temporary`、`delete_after_read` |
| `retention_seconds` | 临时模式缺省或 `0` 时为 `7200` | 保存成功后的保留秒数，非零值为 60–31536000；2 小时为 7200，3 天为 259200。legacy 模式不能指定非零值。 |

## 三种模式

- **legacy / 不传参数**：沿用现有 Local/S3 存储和运营方的全局清理配置；没有全局保留期时，不主动删除历史结果。
- **temporary**：按每张图片保存成功的时间计算过期时间，读取不提前删除。适合调用方需要下载重试或恢复未完成任务的场景。
- **delete_after_read**：通过下述专门的 Base64 交付接口完整写出结果后清理；或者下载原图后调用确认接口清理。没有取回的结果仍按 `retention_seconds` 到期回收。

```json
{
  "model": "gpt-image-2",
  "prompt": "A cat sleeping in sunlight",
  "n": 1,
  "storage_mode": "temporary",
  "retention_seconds": 7200
}
```

需要取回即清理时，将 `storage_mode` 改为 `delete_after_read`。需要保留 3 天时，将 `retention_seconds` 改为 `259200`。省略两个参数即可恢复旧行为。

临时模式仍需要保存图片文件/对象，异步调用方才能稍后取回；它不会将图片 Base64 写入数据库。任务参数记录存储策略，图片记录增加 `expires_at`（Unix 秒）和 `delete_after_read`。图片的策略与元数据一起写入；历史行默认 `0/false`，不会改变旧图片的保留期。

显式图片保留期优先于全局 `IMAGE_ASSET_RETENTION_DAYS`，不被全局清理提前删除。后台每分钟扫描到期图片，单次最多处理 1000 张；积压、存储错误或服务停机可能延后物理删除。到期后的原图、缩略图和内联缓存请求立即停止提供图片。删除失败保留已过期的元数据，下一次继续清理。

## 查询、交付与确认

1. 使用现有 `GET /v1/images/jobs/:id/result` 或批量查询轮询状态。新模式的 `assets[]` 增加可选的 `expires_at`、`delete_after_read` 字段；旧模式不增加这些字段。
2. 任务完成后选择原有 `assets[].proxy_url` 下载，或 `GET /v1/images/jobs/:id/output` 读取 Base64。两个新增接口均要求创建任务时的同一 API Key；其他 Key 返回 404。
3. 使用 URL 下载且选择 `delete_after_read` 的调用方，应在自己可靠保存图片后 `POST /v1/images/jobs/:id/ack`。成功返回 204，可重复确认。只删除该任务标记了 `delete_after_read` 的图片，不删除 legacy/temporary 图片或任务历史。

`GET /v1/images/jobs/:id/output` 返回：

```json
{
  "job_id": 42,
  "data": [{"id": 71, "mime_type": "image/png", "width": 1024, "height": 1024, "b64_json": "..."}]
}
```

该接口逐张流式编码，不在内存聚合多张 Base64。queued/running 返回 409；结果已过期、已清理或不存在可交付图片时返回 410。部分成功任务返回现存的输出；请结合状态查询中的 `warning` 判断是否生成了全部图片。失败/中断的写出不会主动清理剩余有效图片。

HTTP 完整写出不能证明客户端已经落盘：若必须保证调用方持久化成功后再清理，使用 **原图 URL 下载 + ack**。不要用可自动消费的 `/output` 做无条件自动重试。状态轮询、旧详情查询、原图 URL 和缩略图预览均不自动消费图片；自动删除仅发生在明确交付/确认时。过期或消费后不会重新生图或触发额外计费。

## 批量任务与兼容性

`POST /v1/images/jobs/results`（兼容别名 `/result`）接收 `{"ids":[1,2,3]}`，也兼容 `job_ids`。最多 500 个 ID、32 KiB 请求体，重复 ID 去重；未找到或不属于当前 Key 的 ID 在 `missing_ids` 返回，不能据此区分任务是否属于其他人。返回结果不包含输入图、提示词或 Base64。1000 个任务可以拆成两批查询。

1000 个任务应入队执行，不应在 2 GiB 服务器同时解码 1000 张图片。队列和内存阶段限制仍需显式配置，例如 `IMAGE_JOB_WORKERS=10`、`IMAGE_JOB_MEMORY_WORKERS=1`，并根据图片大小和上游配额测量峰值。API Key、账号和作用域并发限制继续生效；未启用队列时旧接口的接收行为不变。

数据库迁移只增加两列与过期索引，支持 SQLite/PostgreSQL。回退旧二进制可保留新列，但旧程序不会执行新的到期/消费策略，需先清理或另行安排清理这些图片。应用之外的备份、S3 版本历史和调用方自己的文件不由本功能删除。
