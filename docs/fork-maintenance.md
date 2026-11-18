# Fork 维护台账

本台账记录本 fork 相对原始上游的功能、修复和维护差异，供后续开发与同步上游时核对。项目独立维护，公开内容只包含本仓库自身的行为、代码归属与验证证据。

## 维护来源与基线

| 对象 | 地址或版本 |
| --- | --- |
| 维护仓库 | [Syh1906/sub2api](https://github.com/Syh1906/sub2api) |
| 原始上游 | [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) |
| 当前同步分支 | `main` |
| 本次上游基线 | [`15871f904056f548dd874a1d069ffc6cdf777b78`](https://github.com/Wei-Shaw/sub2api/commit/15871f904056f548dd874a1d069ffc6cdf777b78) |

## 上游同步记录

2026-11-18：从原始上游 `main` 快进同步。维护仓库原提交为 `cc6a8e517448d9b1e971be27ab799d1454400761`，无独有提交；同步后与上述上游基线一致，保留原有历史，没有强制推送。

本次只完成源码与Git历史同步，未构建、发布或部署，也未执行数据库迁移。同步内容含上游新增迁移源码，运行环境升级前仍需按实际版本验证迁移、接口和数据保留，不能把同步成功等同于业务验收。

## 自有功能与修复

### 原生请求编号、翻页与 Key 写入核对（待发布）

- 客户请求追踪以响应头 `X-Client-Request-Id` 查询常规网关及原生文本补全的新增成功记录。用量详情的 `customer_request_id` 与追踪列表一致，原 `request_id` 保留计费编号，重复的客户编号不合并扣费。迁移 252 为 `usage_logs` 增加可空字段。历史记录保留可以从原编号确定的值，旧原生记录只能使用已存的计费编号。
- 客户请求列表返回 `next_cursor`，下一页传入 `after` 与原筛选条件。按时间、记录类型与记录主键定位下一条，新增在列表顶部的请求不会挤动继续向后读取的位置。原有分页参数仍可使用。
- 管理员可通过 `GET /api/v1/admin/api-keys/:id/access` 核对单把 Key 的 ID、所属用户 ID、总额度、已用额度和状态。迁移 253 为仅改额度的操作增加持久缓存清理任务，沿用现有处理器；按请求累计用量不会触发该任务。
- 隔离 PostgreSQL 与模拟文本上游验证了响应头精确查询、不同客户的记录隔离、两笔独立记账、插入新请求后继续翻页及管理员单 Key 读取。后续同步上游时需保留独立计费编号、历史编号解释与额度缓存失效规则。

### 管理员 Key 额度、启停与用户请求记录（2026-11-18，v0.2.22-fork.2）

- 管理员通过既有 `PUT /api/v1/admin/api-keys/:id` 指定 `quota` 或正数 `quota_increase`，同时提交刚读取的 `expected_quota`。额度已变化时返回 409，客户端应重新读取。零总额度代表不限额，只有有限额度允许增量调整。启停使用 `status=active|inactive`。分组和限流重置沿用原接口，独立于额度调整。操作进入现有管理员审计。
- 总额度变更与扣量按同一条 Key 记录串行保存，历史消耗保持原值。重新启用时重新判断有效期和剩余额度；额度、状态成功更新后清除该 Key 的鉴权缓存。
- 已登录用户通过 `GET /api/v1/usage/requests` 查询自己的成功与失败记录。支持 `request_id` 精确匹配、日期、客户请求的模型、状态及分页，默认最近七天。列表以 `kind` 和 `id` 定位现有详情，时间相同的记录次序固定。失败记录继续遵循用户错误查看开关。没有采集耗时的记录返回空值。成功用量和失败日志各自沿用原有保留期限。
- [v0.2.22-fork.2](https://github.com/Syh1906/sub2api/releases/tag/v0.2.22-fork.2) 的 [Linux amd64 发布](https://github.com/Syh1906/sub2api/actions/runs/44306627042)已完成，源码为 `2f6ffa8ba439ad17b32e6671a358f955a204d746`，镜像索引摘要为 `sha256:ccfb3cb156fa4629c82a9179f21a20c1f5098a7d93c493489516d0455e0bb140`。本次 Go 单元与集成测试、静态检查、安全扫描均通过。GitHub 的 macOS 运行机未能分配，脚本作业未执行。
- 本机候选源码和按摘要拉取的正式镜像分别用两个隔离用户、PostgreSQL 和模拟模型服务验证。并发两次对同一基数加额只成功一次，停用、到期和额度耗尽均在实际模型入口拒绝；按响应头编号查询、同一时刻跨页、错误隐藏和跨用户详情检查通过。
- 代码位于 `internal/service/admin_api_key_access.go`、`internal/handler/user_requests.go` 和 `internal/repository/ops_repo_request_details.go`（相对 `backend/`）。没有数据库迁移。

### 原生标准客户端端点（2026-11-18，v0.2.22-fork.1）

- 新增旧版 Completions、标准音频、Moderations、Rerank 与 Decisions 的固定原生入口和根路径别名。复用客户身份、分组权限、并发、调度、余额与用量记账；不引入独立账本。
- 音频上传保留文件字节，音频响应保留媒体类型。Completions 保留原生文本/流式结构。音频与审核要求明确按次价格，Token 计费要求合法上游用量；不默认免费或估算伪造用量。
- 归属：`backend/internal/handler/openai_native_protocol.go`、`backend/internal/service/openai_native_protocol.go`、`openai_native_forward.go`，以及账号能力、路由和现有记账入口。无数据库迁移；接口与配置见[原生扩展协议](native-protocols.md)。
- 定向验证通过：7 个端点的原生转发、Completions SSE 与截断、multipart 文件/模型映射、音频/文本媒体类型、无身份拒绝、根路径与 `/v1` 模型白名单、按 Token/按次价格和计费来源、缺失价格/用量拒绝、上游 400 保留。合成账务测试中两次独立执行各扣 0.25，余额缓存 100 → 99.5；重复客户端编号不能合并账单，队列溢出仍执行必要记账。
- 验证使用 `GOMAXPROCS=2`、`go -C backend test -p=1 -tags=unit ./internal/service ./internal/handler ./internal/server/routes -run TestNativeProtocol` 及受影响的既有记账队列用例，使用合成上游和账务存储替身。没有真实模型/费用，也不把测试替身视为数据库整体联调。
- 审核的默认模型也进入白名单；拒绝重复或大小写歧义的路由和流式字段。Completions 需有输入和输出用量，自定义 Token 价格要明确对应方向；Rerank 与 Decisions 只读输入计量字段。
- 标准语音合成支持增量二进制或 SSE，转录与翻译可按上游能力转发 SSE。按次记账以对应音频完成事件或正常二进制结束为准，断流不扣费。归属：`backend/internal/service/openai_native_audio.go`、原生处理器及 HTTP 错误收尾。隔离 HTTP 与 PostgreSQL 已核对取消后成功、缺终止、二进制截断、Key 额度及订阅窗口；未涉及真实供应商、迁移或发布。
- 实现提交 `de5e184c2`，流式传输与用量修复提交 `887dd88da`。真实供应商能力与费用须按实际渠道另验。
- [v0.2.22-fork.1](https://github.com/Syh1906/sub2api/releases/tag/v0.2.22-fork.1) 已由[精简发布流程](https://github.com/Syh1906/sub2api/actions/runs/44226423818)发布；源码 `bf2e98ec9404b9cce46deb3a7d72696ab965c01a` 的 [CI](https://github.com/Syh1906/sub2api/actions/runs/44224610094) 通过。Linux amd64 镜像 `ghcr.io/syh1906/sub2api@sha256:e01cc225d8e49199fbedb73646d561e43f9edaada5eaed7a70252f7853e1fcb7` 已按摘要拉取并核对源码标签。隔离 PostgreSQL 实测：一笔文本生成和两笔语音各只记一次，客户端收到首段后离开而上游正常完成的语音仍按次计费。未部署到生产。
- 同步上游时逐项核对是否已提供等价端点，特别注意原生流式输出、multipart 模型映射、渠道价格来源、缺失用量处理和重复请求编号不能漏扣费。

后续每项功能或修复记录以下内容：

- 目的与可观察的行为差异，包括影响的接口、配置、权限或计费语义。
- 涉及模块与主要文件，相关提交和已发布版本；未发布时明确标注。
- 数据结构或迁移影响；无影响时简要说明。
- 实际验证范围、结果与尚未验证的条件，不用模拟结果替代真实服务验证。
- 与上游实现的关系，以及下一次同步时需要保留、调整或重新核对的内容。

公开条目不得包含私有调用方标识、私有链接、本机路径、内部部署信息或凭据；公共引用须指向克隆后可用的仓库文件或可访问的公开资料。

## 后续同步核对

本 fork 的 `v<上游版本>-fork.N` 发布标签默认走已有的精简流程：只发布 Linux amd64 镜像到自己的 GHCR 和对应的 GitHub Release，不向 Docker Hub 发布、也不发送通知。手动发布仍可按工作流参数选择目标。同步上游后保留此标签规则，发布不会自动部署到运行实例。

临时联调可复用已有合适摘要，或手动运行[快照工作流](../.github/workflows/snapshot.yml)：使用默认分支已通过 CI 的完整提交和本 fork 已发布的基线，排除数据库迁移差异。成功后从该运行的 `snapshot.json` 取固定摘要。快照不更新正式标签或 Release。

同步前检查当前分歧与台账中的自有改动，确认目标分支或tag。可以快进时保持原历史；需要合并时逐项解决冲突，不以覆盖已有改动代替整合。

同步涉及数据库、依赖、鉴权、账务或协议行为时，分别核对对应变更与必要验证。若上游已覆盖本fork功能，记录证据及后续处理，不仅凭相似标题删除旧实现。每次同步后更新本表的基线和同步记录；发布或部署结果另按实际完成情况记录。
