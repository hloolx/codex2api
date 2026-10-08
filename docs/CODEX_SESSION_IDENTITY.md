# 会话身份收敛

账号的「设备指纹收敛」新增一个可选模式：**会话身份收敛**。
它与「设备 + 会话」的区别是：不会把同一账号的所有会话合成一个会话。
设备仍按账号固定，但不同对话、子线程各自保留稳定的映射。

例如，下游发来：

```text
父任务：session=root, thread=root
子任务：session=root, thread=child, parent_thread_id=root
另一个对话：session=other, thread=other
```

同一账号下，映射后分别是：

```text
父任务：session=A, thread=A
子任务：session=A, thread=B, parent_thread_id=A
另一个对话：session=C, thread=C
```

这里 session、thread 和父线程引用使用同一个映射空间。父引用不会因为字段名不同
被算成另一个 ID；映射也不依赖当前并发数、访问令牌或请求顺序。

## 使用

在账号编辑、快捷配置或批量设置中选择「会话身份收敛」。设置页也可以将它设为
新账号默认模式；只影响以后新增的账号，不批量修改已有账号。

API / 凭据值为 `single_machine_multi_window`。展示名称改为「会话身份收敛」，
存储值及 ID 派生种子保留原实验实现的取值，避免已有配置失效或身份变化。
旧的 `off`、`device`、`session`、`full` 行为不变，默认仍是 `off`。
只作用于原生 Codex 账号，中转等账号不启用。

## 请求和连接

- 同时处理 HTTP、compact、WebSocket 的会话头及已有 `client_metadata`。
  只有请求体携带身份时，先在请求级头副本中提取，再统一改写；不修改调用方的头。
- WS 握手身份在连接建立时固定，所以连接池在原有 API Key / 模型分区之内，
  再按映射后的 session/thread 分区。同一线程仍能复用连接，不同线程不会共用冻结的身份。
  现有 `previous_response_id` 连接亲和逻辑保留。
- 缺少 session/thread 时不猜测身份，也不从用户内容或 `prompt_cache_key` 派生身份。
  请求中不存在的元数据不会凭空补出。
- 不改写 `prompt_cache_key`、`previous_response_id`、`turn_id` 或轮次时间。
  保留实际 subagent 元数据，不制造子智能体，也不把对话塞进固定数量的窗口。
- 保留账号自定义头的最终覆盖优先级。显式 `CODEX_SESSION_HEADER_MODE=legacy`
  仍使用旧头名，只在新模式下对齐对应值。

这不是网络出口或配额设置，也不能据此保证上游缓存、账号可用性或模型质量。
网关自身的 prompt-cache 分区不变；没有验证上游是否还使用其他身份字段参与缓存。

## 存储与验证

模式值超过原来的 20 字符，新账号默认模式的 PostgreSQL 列扩为 `VARCHAR(64)`；
已有值不变，SQLite 的 TEXT 列无需迁移。降级到不认识该模式的版本前，请先把选中
该模式的账号和新账号默认设置切回旧模式；不需要收窄列。

本地测试覆盖父子引用、并发稳定性、缺失/请求体身份、HTTP/compact 实际出站、
WS 握手和帧对齐、连接分池及复用、管理端保存/批量修改和 SQLite 默认值持久化。
测试使用合成数据和本地 HTTP/WS 服务，不调用真实模型。
PostgreSQL 列扩宽尚未在真实实例上验证。

设计时对照过 `DeanZFC/sub2api-custom` 的
`backend/internal/service/openai_codex_fingerprint.go`
（`08ef70d1750e665ede7c2fb2306ef48599428c66`）。
这里没有移植其固定窗口槽位算法，而是沿用本项目的 ID 派生工具，映射客户端已有的关系。
