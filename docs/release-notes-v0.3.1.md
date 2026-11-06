# CommandCode Provider v0.3.1

修复上游额度耗尽返回 HTTP 400 时被当作普通请求错误、不会切换凭证的问题。

## 修复

CommandCode 在额度耗尽时返回：

```json
{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits to continue using the service.","type":"invalid_request_error","code":"BAD_REQUEST"}}
```

`code` 是通用的 `BAD_REQUEST`，无法单独用于路由；插件现在只在该 400 的错误体明确包含 `insufficient credits` 等额度信号，或错误 code 明确为额度错误时，返回 **402 Payment Required**，消息固定为 `upstream quota exhausted`。CPA 因此把当前凭证标记为 `payment_required`、冷却 30 分钟，并在当前请求中切换到下一个 auth。

- 普通 400（模型不存在、参数错误、格式错误等）仍按请求错误返回，不换凭证。
- 覆盖非流式、Provider SSE 打开和 CLI 打开路径。
- 流式 400 仅在 `Content-Length` 有界且为 JSON 时读取错误体，最多 64 KiB；读取失败保持原 400，不阻断请求。
- 不把上游错误原文、密钥或请求体写入客户端响应或日志。
- 不修改 CPA、Observer、上游协议、额度算法或 SDK 依赖。

## 升级

替换对应平台的原生库并重启 CPA；保留既有插件设置和上游凭据，不需要重新导入。管理界面和配置格式不变。

## 验证

- 单元测试覆盖真实 CommandCode 400 body、Anthropic credit balance、显式 quota code、普通 400、429/402 不重复映射以及消息脱敏。
- 真实 CPA 宿主集成覆盖 CLI/Provider × 流式/非流式四种组合：额度 400 必须产生 2 次上游调用并切换到另一个 auth；普通 400 只产生 1 次调用且保持 400；全部 auth 额度耗尽时客户端收到 402 和固定消息。
- `make test`、`make race`、`make static`、`make build VERSION=0.3.1`、CPA v8.0.23 与 v8.0.53 的 `make integration` 全部通过。
- 发布 workflow 在 v8.0.23 隔离宿主上重复同一套测试，并构建 linux/amd64、linux/arm64、darwin/arm64 三个原生包。
