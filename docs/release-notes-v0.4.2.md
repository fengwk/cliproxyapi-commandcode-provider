# CommandCode Provider v0.4.2

修正 v0.4.1 对 pi 厂商扩展仍过严的处理。CLI 模式采用“提取支持字段及适配输入 → 校验 → 重建 CommandCode 请求”，而不是遇到未知扩展就返回笼统 400。

## 修复

- Chat、Messages、Responses 的未知顶层扩展先被丢弃，不进入 SDK。pi 的 `tool_stream` 不再阻断请求；忽略意味着不承诺该扩展的控制语义生效。
- 显式 `reasoning_effort` 高于 thinking 兼容层。`reasoning_effort: "max"` 原样保留，不从 `thinking.type` 捏造、替换或降低档位；整个 thinking 对象（包括 `clear_thinking`）不再反向覆盖原生 effort。
- 没有显式 effort 时才尝试 thinking.type 适配：disabled 使用模型支持的 off/none；enabled 不猜测强度。无法表达的预算、非法适配值仍明确拒绝。CPA metadata 原生 effort 与 body 之间的既有优先级不变。
- Chat 的 `max_completion_tokens` 优先于 `max_tokens`，只校验选定后的值；保留字段类型、模型能力及最终 builder 约束仍严格检查，非法 temperature/effort/token limit 不因投影而被吞掉。
- n>1、强制工具选择、structured output、非法消息/工具及 Responses 状态型控制继续拒绝；Provider 直通不受此次 CLI 投影影响。

## 验证与升级

- 编解码与 Provider 测试覆盖字段投影、原生 effort 优先、token-limit 别名选择、已识别非法参数拒绝以及已有语义边界。
- 真实未修改 CPA v8.0.23 + loopback mock 复现 pi 1.5.1 的 GLM 控制集合：`max_tokens=131072`、`reasoning_effort=max`、`thinking.clear_thinking=false`、`tool_stream=true`、13 个工具。流式/非流式均校验原生 effort、token limit 和工具数不变，扩展不进入 generate；超过模型 cap 时本地拒绝且零上游调用。
- 发布门禁仍在 tag 上运行并构建 Linux amd64/arm64、macOS arm64 包。未使用真实密钥或收费推理；真实上游接受性不由本地 mock 保证。
- 升级到 v0.4.2 后按 CPA 提示重载或重启，保留配置与凭据。商店若仍显示旧版，注意 CPA 最新 Release 查询缓存。
