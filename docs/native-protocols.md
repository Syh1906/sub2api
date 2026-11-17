# 原生扩展协议

本 fork 提供以下客户 API Key 入口，使用既有用户、分组、额度、账号调度和用量记账。协议处理仍由所选上游完成，不把请求转换为 Chat Completions。

| 端点 | 请求与响应 | 账号能力名 | 计费要求 |
| --- | --- | --- | --- |
| `POST /v1/completions` | 原生 `prompt`，保留 `choices[].text`，支持普通 JSON 与 SSE | `completions` | Token 或显式按次价格 |
| `POST /v1/audio/speech` | JSON 输入，原生音频字节和媒体类型 | `audio` | 显式按次价格 |
| `POST /v1/audio/transcriptions` | multipart 音频文件，原生转录结果 | `audio` | 显式按次价格 |
| `POST /v1/audio/translations` | multipart 音频文件，原生翻译结果 | `audio` | 显式按次价格 |
| `POST /v1/moderations` | JSON 输入与审核结果 | `moderations` | 显式按次价格 |
| `POST /v1/rerank` | 原生 query/documents 与排序结果 | `rerank` | Token 或显式按次价格 |
| `POST /v1/decisions` | 原生 input 与决策结果 | `decisions` | Token 或显式按次价格 |

这些路径也提供去掉 `/v1` 的根路径别名。固定路由不允许客户端指定任意上游路径。所有请求必须提供模型；音频上传须包含 `model` 和 `file` 字段。流式参数仅对 Completions 开放。上游须实际支持目标端点，仅把账号标为某种能力不会自动增加供应商功能。

## 配置

1. 创建 OpenAI 兼容 API Key 账号，填写其 Base URL、上游 Key 和实际模型映射。OAuth 账号不能承接这些新增端点。
2. 如账号已有 `credentials.openai_capabilities` 限制，加入表中的能力名；未配置时沿用账号现有的不限制行为。此配置通过现有账号接口管理，不新增管理页面。
3. 将账号关联到客户分组，配置模型权限、倍率与价格。客户调用使用自己的 API Key，不使用管理员 Key 或上游 Key。
4. 对音频和审核模型，在分组或渠道的模型价格中设置 `billing_mode: per_request` 和明确的 `per_request_price`。价格为每次成功请求的金额，零值仅表示管理员明确配置免费。不能只设置计费模式而遗漏价格。本文不设默认音频单价，不把音频字节或字符当作文本 Token。

客户模型名、渠道映射名和账号上游模型名按既有映射顺序处理。价格按渠道的 `requested`、`channel_mapped` 或 `upstream` 计费模型来源选择，并在请求执行前确定。新增端点暂不支持 `response_model` 定价；需先改用上述已支持的来源，不能静默换一种扣费语义。本次按次音频实现不包含按秒、按字符或音频 Token 的价格换算。

## 失败与记账

- 请求仍经过 API Key、分组模型白名单、余额/订阅、用户及账号并发校验。缺失价格会在请求上游前明确失败。
- Token 计费的上游必须返回合法 `usage`。支持 `prompt_tokens`/`completion_tokens`、`input_tokens`/`output_tokens`，以及排序服务的 `total_tokens`。缺失或非法用量不伪装为零 Token 成功。
- Completions 流强制请求 `stream_options.include_usage`，转发原生文本分片并提取最终用量。流中断不会伪造 `[DONE]`；已经观察到的用量仍交给现有账务流程。客户端断开后沿用既有上游排空以收集用量的语义。
- 每次执行使用独立的计费请求编号；客户端重复提供同一请求编号不能把两次执行合并为一次扣费。账务仍使用既有幂等写入流程，不另建账本。
- 源码协议处理和合成上游测试不能替代运行镜像联调。供应商真实能力、收费规则和实际凭据应在隔离环境另验；源码支持不等于已经发布或部署。
