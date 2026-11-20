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

## CI 运行平台

推送、拉取请求和使用默认选项手动运行 [CI](../.github/workflows/backend-ci.yml) 时，作业使用 Linux 运行机。通用部署检查覆盖 Docker Compose 安全配置、网关环境变量、容器运行资源和 Caddy 缓存配置。

出现明确的 macOS 部署需求时，再单独安排平台接入与验收。需要验证现有 Apple 容器部署脚本时，在 GitHub Actions 的 CI 页面选择 **Run workflow** 并勾选「运行 macOS 专属部署检查」。只有该次操作会为这项检查申请 macOS 运行机。

## 自有功能与修复

### Gemini 显式缓存创建费用结算（v0.2.22-fork.11）

创建显式缓存时，上游返回实际 token 数之后，服务会结算本次输入费和所选有效期的存储费。旧版本在从预估金额切换到实际金额时过早释放数据库中的在途使用记录，可能出现创建成功、费用流水实扣为零。修复后，金额预留按实际报价替换，在途记录保留至计费任务完成。若余额不足以保存缓存，既有清理流程仍只结算已发生的输入费。回归覆盖正常创建和余额不足两种情况，无需数据库迁移。[创建计费回归](../backend/internal/handler/gemini_cached_contents_billing_test.go)

### Gemini 兼容渠道的单模型详情（v0.2.22-fork.10）

普通 Gemini 入口支持使用模型目录返回的完整名称读取详情，例如 `GET /v1beta/models/publisher/model-a`。分组白名单可以填写目录中的完整名称，也可以省略开头的 `models/`。合成路由使用包含发布者前缀的模型名，转发前逐段校验路径。读取自定义 Gemini 兼容渠道时，上游返回的 404 会传给客户端，即使请求的名字也在内置模型表中。本机已用可见范围不同的两个客户分组验证目录、详情、拒绝响应和用量。此项无需数据库迁移。[详情路由回归](../backend/internal/server/routes/gateway_test.go) · [兼容渠道响应回归](../backend/internal/handler/gemini_v1beta_handler_test.go)

### Stripe 原单恢复与双商户通知（v0.2.22-fork.10）

恢复待付单时，服务端先核对原商户绑定。即使加密收银会话保存了原公钥和付款意图，缺少绑定时也不返回这两项参数。客户端应以本次恢复响应为准，仅在服务端返回本单公钥后才使用设备上留存的原意图。

Stripe 通知使用付款意图中的订单编号选择验签商户。订单无法定位到唯一本地商户时，服务返回可重试的错误。双商户的签名、重复通知及账务结果通过[PostgreSQL 集成用例](../backend/internal/payment/integration/payment_precision_postgres_test.go)核对。

### 按原订单选择 Stripe 公开密钥（v0.2.22-fork.9 起）

创建、按购买标识重放或查询、恢复本人待付收银台时，响应的可选字段 `stripe_publishable_key` 对应该订单实际使用的 Stripe 实例。启用付款恢复签名时，新的 Stripe 会话将公开密钥和原付款意图一起保存。升级前的待付订单按原单记录的实例读取公开密钥；未保存付款意图的订单不会因恢复而重新下单。客户端应将同笔订单的公开密钥和付款意图配对，缺少对应密钥时提示核对渠道配置。站点的支付配置接口保留原有字段供既有调用方使用。此项不需要数据库迁移。[双商户回归用例](../backend/internal/service/payment_stripe_checkout_test.go)

### 三位小数支付币种的实付金额（v0.2.22-fork.7 起）

[迁移 258](../backend/migrations/258_payment_order_pay_amount_precision.sql) 将订单实付列从两位小数扩至三位，保留原有 18 位整数范围。升级会保留历史订单已存金额，早期舍入的第三位无法从数据库还原。订单积分面额和退款扣回的积分继续按现有账务精度记录，渠道退款按原单实付和支付币种换算。

[PostgreSQL 集成用例](../backend/internal/payment/integration/payment_precision_postgres_test.go) 覆盖直接支付 KWD 12.345，以及本金 KWD 12.000 加手续费 KWD 0.345。两笔订单均核对渠道请求、数据库回读、恢复原付款意图、签名通知、重放及错误金额。全额退款退回 KWD 12.345，第二笔扣回半数积分时退回 KWD 6.173。测试使用本地模拟 Stripe。

v0.2.22-fork.8 同步修正运营支付统计。总额、当日金额、均价、按天和按方式汇总、客户排行均按支付币种的小数位数返回。两笔 KWD 12.345 和 12.344 的合计为 24.689，均价为 12.345。[统计回归用例](../backend/internal/service/payment_stats_test.go)

### 原单恢复、在途消费与退款核对（v0.2.22-fork.6）

登录客户可在本人待付原单继续尚未发送的收银请求。服务在发往 Creem 前保存发送状态，收到链接后保存商户收银台编号。回执中断时，运营凭商户编号查询原收银台，核对购买标识、商品与数量后恢复链接；若商户确认未创建，可登记凭证关闭原单。已发送而结果未知的旧单也使用这一路径。Creem 回跳用 `local_order_id` 携带本站编号；付款交付仍依赖验签通知。新购买的退款期从签名成交快照中的已付款订单更新时间起算。旧单可凭同笔通知将原履约时间校正到更早的付款时间。[原单恢复](../backend/internal/service/text_checkout.go)

客户申请退回已兑换积分时，服务先核对本人是否仍有余额计费请求在途。迁移 256 让请求获准、异步结算与退款通过同一客户行排序。正常长请求会续期，失联登记仅在过期后由退款回收；已被回收的登记无法再扣款。未兑换的购买仍可单独退款。退款已批准但尚未派发时，运营可续办；明确拒绝时退回本单权益。商户交易仍显示已付款时，运营需取得本次退款最终失败或未受理的凭证，才能通过 `POST /api/v1/admin/payment/orders/:id/refund/resolve-failure` 登记并解冻。系统再次核对原交易、金额及申请时间，并记录操作人。[退款核对](../backend/internal/service/text_refund_reconciliation.go)

客户与运营的订单列表提供兑换状态，只有购买者详情提供仍可领取的兑换码。运营可在 `GET /api/v1/admin/payment/orders/:id/usage` 分页读取请求编号、结算时间及该请求从本单扣除的积分。迁移 257 保留历史已用合计，将新的单条及批量结算逐请求分配；没有请求编号的余额调减另行累计。升级已有实例时应先停止旧版接收新请求，让在途计费完成，再切换所有处理退款和计费的进程。[订单明细](../backend/internal/service/text_purchase_detail.go) · [消费归属迁移](../backend/migrations/257_text_purchase_usage.sql)

PostgreSQL 18 与模拟商户覆盖原单断点、通知延迟、退款拒绝和回执丢失、请求在途、跨单消费、防重及新旧数据。真实收退款需使用商户沙箱独立核验。[交易集成用例](../backend/internal/payment/integration/text_purchase_postgres_test.go)

### 单次购买、兑换交付与原单退款（v0.2.22-fork.5）

支持通过 Creem 托管结账购买文字积分兑换码。后端环境使用 `CREEM_MODE`（`test` 或 `prod`）、`CREEM_API_KEY`、`CREEM_WEBHOOK_SECRET`，按金额分别配置 `CREEM_PRODUCT_10`、`CREEM_PRODUCT_20`、`CREEM_PRODUCT_50`、`CREEM_PRODUCT_100`。商品须为对应面额的美元一次性商品。支付总开关启用且该档配置齐全时，登录客户可通过 `GET /api/v1/payment/text-offers` 查看可售档位。创建走原支付订单接口，类型为 `text_voucher`，方式为 `creem`。商户通知入口为 `POST /api/v1/payment/webhook/creem`，需订阅 `checkout.completed` 和 `refund.created`。

付款通知经签名、订单关联及金额核对后，同一笔购买签发一个归购买者所有的兑换码。客户从本人订单详情取码，使用原有兑换入口后才入账。迁移 255 记录每笔购买的已兑换和已用积分。账户余额下降时先消耗普通余额，接着按兑换时间从早到晚分配至购买记录。冻结余额与可用余额互转不会重复计算。历史未关联的余额作为普通余额处理。商品只适用于运营方已配置为仅提供文字服务的客户范围，模型权限仍由实际分组配置决定。

付款后 14 天内、整笔积分未消耗的客户可申请全额退款。申请冻结该笔权益；运营确认后根据原交易编号请求退款，未兑码失效，已兑未用积分只收回该笔。收到明确失败结果时退回冻结的权益，回执未知时仅查询原交易。外部商户自行退款、发生争议或部分退款时须人工对账。PostgreSQL 隔离测试使用模拟商户，验证签名、重复通知、本人兑换、多笔购买消费、超期、未兑及已兑退款、回执中断和兑换退款并发。[交易测试](../backend/internal/payment/integration/text_purchase_postgres_test.go)

### 支付订单防重与收银台恢复（v0.2.22-fork.5）

登录客户创建订单时可携带 `Idempotency-Key`。同一客户重复提交同一购买内容时返回原单，改换金额、方式或套餐会返回冲突。未携带标识的原有客户端可照常下单。迁移 254 为购买标识摘要建立客户内唯一约束，并为需要恢复的客户端支付参数预留加密列，既有订单可保留空值。

`GET /api/v1/payment/orders/purchase` 使用当前客户身份和原请求头查询，`GET /api/v1/payment/orders/:id/checkout` 读取本人的收银台。收银信息中的 `purchase_recovery` 和客户订单中的 `checkout_recovery` 用于识别服务端能力。待付且未过期的订单可恢复已有链接或二维码。支持签名密钥的实例在创建 SDK 会话时加密保存客户侧参数，更换设备后仍须满足支付商自身的有效期和使用条件。终态和过期订单只返回订单结果。服务端不因读取恢复接口再向支付商下单。已在 SQLite 检查并发创建、内容冲突、本人查询及终态收银限制，并用 PostgreSQL 18 验证迁移保留旧订单及客户内唯一约束。[订单恢复实现](../backend/internal/service/payment_checkout_recovery.go) · [定向测试](../backend/internal/service/payment_checkout_recovery_test.go)

### 带来源的模型基础参考价（v0.2.22-fork.4）

管理员通过 `GET /api/v1/admin/channels/model-reference-price?model=…` 查看指定型号的厂商目录参考。精确匹配且引用对应厂商资料时返回 `data.reference`，内含模型名、来源链接及有依据的每 Token 美元单价；未找到时为 null。模型广场的对应条目可含同一份 `vendor_reference`。原有查价接口及扣费逻辑继续提供既有行为。

服务在应用运营价覆盖前保留参考字段，按精确目录键查找。缺失的价格字段不返回，厂商列出的零值保留为 0；运营自建条目和通用系列回退不能作为厂商参考。已核对 [GPT-4.1 的标准文本价格](https://developers.openai.com/api/docs/models/gpt-4.1)，为随附目录中相应两条记录补充来源。实现与回归用例见 [价格读取](../backend/internal/service/vendor_price_reference.go)、[参考价测试](../backend/internal/service/vendor_price_reference_test.go)。无数据库迁移。

### 原生请求编号、翻页与 Key 写入核对（2026-11-19，v0.2.22-fork.3）

- 客户请求追踪以响应头 `X-Client-Request-Id` 查询常规网关及原生文本补全的新增成功记录。用量详情的 `customer_request_id` 与追踪列表一致，原 `request_id` 保留计费编号，重复的客户编号不合并扣费。迁移 252 为 `usage_logs` 增加可空字段。历史记录保留可以从原编号确定的值，旧原生记录只能使用已存的计费编号。
- 客户请求列表返回 `next_cursor`，下一页传入 `after` 与原筛选条件。按时间、记录类型与记录主键定位下一条，新增在列表顶部的请求不会挤动继续向后读取的位置。原有分页参数仍可使用。
- 管理员可通过 `GET /api/v1/admin/api-keys/:id/access` 核对单把 Key 的 ID、所属用户 ID、总额度、已用额度和状态。迁移 253 为仅改额度的操作增加持久缓存清理任务，沿用现有处理器；按请求累计用量不会触发该任务。
- [v0.2.22-fork.3](https://github.com/Syh1906/sub2api/releases/tag/v0.2.22-fork.3) 的 [Linux amd64 发布](https://github.com/Syh1906/sub2api/actions/runs/44345806587)已完成，源码提交 `2a9457bb1ae5d6a35a80e9c759c6ebaf2440000c`，镜像索引摘要 `sha256:9339eb5c7a7ef4a1425e2c868e3a1d7c0879886a81bf4dc68bbdf9820f86fef2`。该提交的 Linux 单元与集成测试、前端、静态检查、发布脚本检查及安全扫描均已通过。
- 按摘要启动正式镜像完成两项迁移，隔离 PostgreSQL 与模拟文本上游验证了响应头精确查询、不同客户的记录隔离、两笔独立记账、插入新请求后继续翻页、同刻混合分页、单 Key 核对与额度更新任务。后续同步上游时需保留独立计费编号、历史编号解释与额度缓存失效规则。

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
