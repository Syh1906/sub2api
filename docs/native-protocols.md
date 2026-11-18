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

这些路径也提供去掉 `/v1` 的根路径别名。除审核请求可省略 `model` 并使用 `omni-moderation-latest` 外，其余请求必须提供模型；默认审核模型同样参与分组白名单与定价。上传音频使用 multipart 的 `model` 文本字段和 `file` 文件字段。上游必须实际支持所选模型、端点及响应格式。

语音合成省略 `stream_format` 时返回完整音频。传 `"audio"` 时逐段接收音频字节；传 `"sse"` 时接收原生语音事件，以 `speech.audio.done` 结束。转录或翻译请求可传表单字段 `stream=true` 逐段接收文本，以 `transcript.text.done` 结束；翻译是否支持流式取决于供应商。排队不会把心跳写进音频。

## 配置

1. 创建 OpenAI 兼容 API Key 账号，填写其 Base URL、上游 Key 和实际模型映射。OAuth 账号不能承接这些新增端点。
2. 如账号已有 `credentials.openai_capabilities` 限制，加入表中的能力名；未配置时沿用账号现有的不限制行为。此配置通过现有账号接口管理，不新增管理页面。
3. 将账号关联到客户分组，配置模型权限、倍率与价格。客户调用使用自己的 API Key，不使用管理员 Key 或上游 Key。
4. 对音频和审核模型，在分组或渠道的模型价格中设置 `billing_mode: per_request` 和明确的 `per_request_price`。价格为每次成功请求的金额，零值仅表示管理员明确配置免费。不能只设置计费模式而遗漏价格。自定义 Token 模型没有已知基础价时，Completions 要明确输入和输出两项单价；Rerank、Decisions 要明确输入单价。零值可以显式配置，但缺失价格不能当成免费。本文不设默认音频单价，不把音频字节或字符当作文本 Token。

客户模型名、渠道映射名和账号上游模型名按既有映射顺序处理。价格按渠道的 `requested`、`channel_mapped` 或 `upstream` 计费模型来源选择，并在请求执行前确定。新增端点暂不支持 `response_model` 定价；需先改用上述已支持的来源，不能静默换一种扣费语义。本次按次音频实现不包含按秒、按字符或音频 Token 的价格换算。

## 失败与记账

- 模型、流式控制及 `include_usage` 的重复字段或大小写歧义会被拒绝，保证准入、转发和记账采用相同请求含义。
- 请求仍经过 API Key、分组模型白名单、余额/订阅、用户及账号并发校验。缺失价格会在请求上游前明确失败。
- Token 计费的上游必须返回合法 `usage`。Completions 要有完整的输入和输出计数；Rerank 按输入或 `total_tokens` 计量，Decisions 只按 `input_tokens` 计量，其余输出元数据不作为消费用量。缺失、部分缺失或非法用量不伪装为零 Token 成功。
- Completions 流强制请求 `stream_options.include_usage`，转发原生文本分片并提取最终用量。流中断不会伪造 `[DONE]`；已经观察到的用量仍交给现有账务流程。客户端断开后沿用既有上游排空以收集用量的语义。
- 每次执行使用独立的计费请求编号；客户端重复提供同一请求编号不能把两次执行合并为一次扣费。账务仍使用既有幂等写入流程，不另建账本。
- 音频流按次收取：SSE 必须收到对应完成事件；二进制必须收到音频并正常结束。供应商返回错误、SSE 缺少完成事件或二进制断流均不记成功费用。二进制输出开始后遇到断流，客户端收到传输错误。客户端主动离开时，若上游仍在既有收尾时限内完成，照常计一次。
- 源码协议处理和合成上游测试不能替代运行镜像联调。供应商真实能力、收费规则和实际凭据应在隔离环境另验；源码支持不等于已经发布或部署。
