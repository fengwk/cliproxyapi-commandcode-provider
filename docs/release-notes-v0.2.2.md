# CommandCode Provider v0.2.2

本次修复审查确认的协议、流生命周期与管理界面问题。不修改 CPA 核心、凭据模式、配置格式或调度逻辑，不增加依赖。

## 修复

- 跨协议转换前识别 Responses 的明确失败、取消及非终态状态，返回固定脱敏 502，不再把它们包装为成功回答。原生同协议 Responses 仍按既有契约透传。
- SSE 支持 LF、CRLF、单 CR 及分块换行，剥离流首 UTF-8 BOM，不吞首帧；拒绝非对象 JSON data。
- `finish_reason` 后仍检查 EOF 尾帧完整性，截断的 usage 帧不再被忽略；合法 `finish_reason` 加干净 EOF、缺少 `[DONE]` 的成功契约保持不变。
- quiesce/shutdown、下游发送失败或取消以明确错误关闭流；上游已经标记完成、但下游终态尚未送达的窗口也不会误报成功。Provider 与 CLI worker 均有回归覆盖。
- 拒绝尾随 `?` 的 `base-url`，避免把上游 endpoint 拼入 query。
- 管理界面的非法 JSON、异常 keys 响应及 `null`/`false`/`0` 显示固定本地错误并保留旧列表，不再误报空目录；合法 `files: []` 仍正常显示为空。

## 升级

- 在插件商店刷新并升级到 v0.2.2，保留既有配置与凭据，无须重新导入或修改 CPA。
- 每凭据 `cli`/`provider` 模式、CPA 选凭据与重试职责、无密钥浏览器持久化约束保持不变。

## 验证与边界

- Go test/vet/race、原生构建、58 项 Node UI 测试、静态及打包安全检查通过。
- CPA v8.0.23/v8.0.24 真实宿主使用本地合成上游的集成测试通过；v8.0.15 不支持所需 schema 6，不在支持范围。
- 13 项既有 Chromium 检查通过，另与 OpenCode Provider 合计完成 36 个 keys 异常响应及刷新保留案例。
- 发布工作流对准确 tag 重新验证，并构建 linux/amd64、linux/arm64、macOS/arm64 安装包及 checksums.txt。
- 所有运行时验证只使用合成凭据、loopback 服务与隔离浏览器上下文，不代表对真实 CommandCode 套餐、缓存或扣费行为的验收。
