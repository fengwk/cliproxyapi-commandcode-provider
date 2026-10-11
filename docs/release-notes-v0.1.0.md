# CommandCode Provider v0.1.0

CLIProxyAPI v8 原生 CommandCode provider 首版。首批验证宿主为 CPA v8.0.23，CLI 兼容性基线为 1.79.2；不 fork、不修改 CPA 核心。

## 功能

- 显式 `cli`（默认 `/alpha/generate`）与 `provider`（`/provider/v1`）双模式，不自动降级或切换。
- OpenAI Chat、Anthropic Messages、OpenAI Responses 三协议流式与非流式；text、inline image、function tools、reasoning 及 usage 投影。
- 宿主选凭据/亲和/冷却/故障转移；UUIDv5 按选定 AuthID 与 canonical session 隔离上游 thread，无插件侧影子调度器。
- CLI wire header profile、ZDR opt-in、合成 workspace、`pause_turn` 续跑及累计 usage。
- 中文同源管理 UI、密钥导入/列表/删除、手动模型配置、单凭据只读配额。
- 三平台原生 ZIP、`checksums.txt`，发布前校验版本、提交、SDK、动态库架构与归档内容。

## 验证

- Go test/vet/race、UI 42 项测试、交付与资产校验测试。
- CPA v8.0.23 + 本地 loopback 合成上游：20 项顶层测试、36 单元协议矩阵，另含亲和/故障转移/配额/UTF-8/截断/取消/关停。
- 发布流程重新对准确 tag 执行验证，随后在 linux/amd64、linux/arm64、macOS/arm64 原生 runner 构建。

## 使用与限制

- 推荐通过本仓库 `registry.json` 自定义源安装；详见 README。Linux 产物依赖 glibc，不支持 musl/Alpine。
- Go 套餐没有 Provider API 权限，应显式使用 CLI 模式；不会通过自动降级隐藏 `403 upgrade_required`。
- 全部上游验收为合成数据；未使用真实凭据、未调用真实推理/billing，真实 workspace 接受性、配额 schema、缓存效果与扣费未验收。
- 原生 ABI 无法无损传递 Retry-After/Retryable，以及已输出内容后的流式错误状态码。
- Observer v0.4.4 尚不支持 CommandCode 精确 cohort，非零费用仍为未定价；不猜测可定价分区。
- CLI transport 不是官方支持的第三方 API。使用前确认条款/授权，只使用有权使用的凭据；不分发官方 CLI 源码或系统提示词。
