# CommandCode Provider v0.4.1

修复 pi 等 OpenAI Chat Completions 客户端发送 DeepSeek 风格 `thinking` 时，CLI 模式在本地返回 `request field is not supported by the CLI wire` 的兼容性问题。

## 修复

- `thinking: {"type":"enabled"}` 搭配模型支持的开启档位（例如 `reasoning_effort: "max"`）现在接受，只向 `/alpha/generate` 的 `params.reasoning_effort` 写入该档位。
- `thinking: {"type":"disabled"}` 未声明 effort 时，按模型能力选择 `off`，没有 off 则选择 `none`；模型均不支持时明确拒绝。显式声明的合法关闭档位保持不变。
- 开关与 effort 冲突时返回明确的本地 400；原始请求与 CPA 最终 metadata effort 均校验，防止后者反转开关意图。
- enabled 没有 effort、固定 budget、未知 thinking 控制/类型以及模型不支持的 effort 仍拒绝。不猜测开启强度，不自动升降档，不采用“忽略所有未知字段”的策略。
- 错误说明 thinking 字段的具体问题，不回显任意控制值、请求正文或密钥。

## 范围与升级

仅改变 CLI 模式的 Chat 兼容输入；Anthropic Messages、Responses、Provider 直通、身份与网络指纹、模型快照、配额规则及 SDK 版本不变。不需要修改 pi 配置或重新导入凭据。

升级到 v0.4.1 并按 CPA 提示重载或重启。CPA v8.0.23 的商店最新 Release 查询成功结果缓存一小时；刚发布时若还显示旧版本，可等待缓存到期，或在允许服务中断时重启 CPA 后重新检查更新。

## 验证

- 定向单元测试覆盖抓包中的 enabled/max、off/none 能力映射、冲突、预算及未知输入拒绝、宿主 metadata 覆盖与最终 wire。
- 真实未修改 CPA v8.0.23 + loopback 合成上游覆盖 Chat 流式/非流式 × enabled/disabled，确认只发送正确 effort、不发送 thinking，且严格请求头契约不变。
- 发布工作流在准确 tag 上执行既有门禁，构建并校验 linux/amd64、linux/arm64、darwin/arm64 三个平台安装包及 checksums。
- 未使用真实密钥或调用收费推理端点；真实上游接受性不由本地 mock 保证。
