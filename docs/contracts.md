# 首版实现契约

`docs/design.md` 是研究方案；本文固定并行切片的公共接口。生产代码只用公共 CPA v8.0.23 SDK，不用 CPA internal。

## 1. CommandCode 编解码包

路径 `internal/commandcode`，类型已在 `types.go` 锁定。编解码包不调用 HTTP、不读取文件、不调度凭据、不管理会话表。

需要提供以下真实实现：

```go
func ValidateClientRequest(body []byte, protocol string, caps Capabilities) error
func BuildRequest(anthropicPayload []byte, opts RequestOptions) ([]byte, error)
func RequestHeaders(key, threadID string, zdr bool) (http.Header, *pluginapi.HTTPWireProfile, error)
func NewDecoder(reader io.Reader, contentEncoding string) (*Decoder, error)
func (d *Decoder) Next() (Event, error)
func (d *Decoder) Close() error
func (u Usage) Add(other Usage) (Usage, error)
func NewResponse(model string) *Response
func (r *Response) Apply(event Event) ([][]byte, error)
func (r *Response) Completion() ([]byte, error)
```

- `ValidateClientRequest` 检查原始 OpenAI Chat / Anthropic / Responses 请求，protocol 为 `openai`、`claude`、`openai-response` 或 SDK 别名 `codex`。拒绝无损转换不了的语义字段/part/tool；不主动取远端图片或文件。允许显式 auto tool choice、n=1、普通文本 response_format；固定 thinking budget 无法映射 effort 时返回明确不支持。
- `BuildRequest` 只接受经过 SDK 转换的 Anthropic messages/tools/system；使用客户端的内容，不复制 CLI 系统提示词。ThreadID 必须有效 UUID。合成目录 `/workspace/cpa/cpa-project`，UTC 日期，空 Git metadata，`memory/taste/skills=null`，`permissionMode=standard`，params.stream 恒 true。max_tokens 默认 64000 并校验明确 cap；temperature/effort 仅合法时发出。省略顶层 mode/promptCache。
- `RequestHeaders` 固定绑定 catalogdata.CLIVersion；按抓包提供 WireProfile，HTTP1Only+DisableAutoCompression。taste=false；ZDR opt-in；traceparent 随请求生成且不进入 body；Content-Type 采用当前抓包的合并值。
- Decoder 是当前上游轮次的有界 NDJSON reader，支持 gzip/deflate/UTF8/CRLF/尾行。`Next` 返回经过验证的 Event；未知扩展事件可以跳过。EOF 前没有 finish/abort 时返回 typed 502；error/abort 返回 typed error。tool-result/providerExecuted 无法无损表达则明确不支持，不伪造客户端 call。
- Decoder 按当前轮保存 cache-write/provider metadata 补充，finish 时交叉校验 usage，不重复加。finish 返回规范 Usage；冲突、负数、溢出不修正。无法证明缓存分区完整的数据不能伪造可定价桶，返回可识别 accounting error（首版 fail-closed）；无合法 usage 不能默认补全部 0 成功。
- `Usage` 的 input/output 为 inclusive，cache read/write 为子集，reasoning 在 output 内。Complete 只在总量/分区自洽时成立。Add 防 int64 溢出，跨轮加一次。
- Response 生成 canonical OpenAI Chat JSON/SSE：text、reasoning_content、function tool_calls、终止原因和完整 usage（含 cache_write_tokens、reasoning_tokens）。Apply 返回 **完整** SSE 帧 byte slices；不用半个 JSON chunk。pause_turn 的 finish 只累加 usage，不发成功 terminal，允许下一轮继续；最终普通 finish 发 stop/tool_calls/length、usage 与 [DONE]。Completion 只有真实完成后可调用，包含相同累计内容/usage。
- Response 仅当前逻辑请求状态；不把消息/工具 schema跨请求缓存。Response model 保持公开 `commandcode/...` 以便宿主与下游识别，generate 请求 model 使用 native ID。

## 2. Native backend

入口固定 `provider.NewManager(provider.NewBridge(callHost))` 和 `HandleCall(method,payload)`，兼容 main.go 的 native shim。

- ProviderID `commandcode`，PluginID `cliproxyapi-commandcode-provider`，展示名 CommandCode Provider，版本变量 `provider.Version` 初始 `0.1.0`。
- config: mode(cli default/provider), base-url(API authority), zdr(false), models(白名单), manual-models。
- manual-models 使用 `{id, protocol?, context-window?, max-output-tokens?, input-modalities?, reasoning-efforts?}`；protocol 是 openai/claude/openai-response，Provider 手动模型必须有已声明 endpoint，CLI 新模型可选 protocol（CLI wire 本身不由 protocol 改变）。
- CLI catalog 默认使用嵌入 CLI/public 交集 87 个；其余不自动暴露。Provider 通过 host 公共目录刷新并原子替换，失败保留 last-good/显示 stale。系统决策模型不能注册成聊天模型。
- auth 导入/list/native parse/prepare/refresh 参考 OpenCode 已验证结构；只读取请求选中的 auth，不自己选 key。
- 只在所属 namespace 的 before-auth hook 将 x-commandcode-session 适配为 CPA affinity。UUIDv5 固定 namespace+JSON([scheme,selectedAuthID,canonicalSessionID])；header=threadID。模型 alias 仍由 CPA 管理。
- 原始请求先 CLI Validate，然后公共 SDK → Anthropic 输入，再 BuildRequest；CLI Response 的 canonical Chat → 所需客户端协议，非流式与流式均经公开 SDK。执行前校验双向 transformer，防止双重转换。
- Provider mode 首选客户端原格式的 declared endpoint，否则采用确定性的可转换路由；HTTP/NDJSON 错误不做 CLI/provider fallback。
- 流式 HTTP open + 有界读到首个可输出 frame/terminal 前置失败后才返回 RPC stream success。先存首帧而不在同步 RPC 中 emit（宿主无缓冲通道，防死锁），异步 worker再发。pause_turn 最多5次续跑，使用同一 body/auth/session/config；不失败重试。
- 所有执行（含非流式与 preflight）使用 host operation_open，HTTP request带 operation_id，管理当前请求资源；cancel/停用/shutdown 可以中断阻塞 read/open。RPC error只保 HTTPStatus，RetryAfter/Retryable 与异步错误状态丢失不能假装已解决。
- 身份/配置/hash元数据不记录 key；error固定码/去敏限长；禁止用 raw upstream body 拼日志。

## 3. 管理 UI 与 quota 的接口

插件注册 resource `/ui`、`/ui.js`、`/ui.css`，资源无密钥，同源 CSP。

管理 API prefix `/v0/management/plugins/cliproxyapi-commandcode-provider`：

- `GET /keys` → `{files:[{name,auth_index,label,status,disabled,unavailable,success,failed}]}`
- `POST /keys` body `{keys:[...],label:"..."}` → `{imported,skipped,failed}`；先校验/去重，最多100keys/64KiB/4096字符，部分失败用207，不能在列表失败时盲写。
- `GET /settings` → `{mode,base_url,zdr,cli_version,catalog:{count,source,fetched_at,stale}, "manual-models":[...]}`
- `POST /validate` → 检查只含 manual-models 的 patch，不保存 config；config保存使用CPA已有插件 config API。
- 删除通过CPA认证管理 API，绝无host.auth.delete；frontend可以复用 OpenCode现有 credential delete。
- quota走CPA原生插件 `/quota` API，按 `{auth_index}` 触发；使用兼容 v0 路径 `/v0/management/plugins/<id>/quota`，标准 v8 页面路径宿主自行注册，不在插件里重复route。
- 标准 QuotaFetchResponse 使用 `subscription`、`summary`、`groups`，JSON字段遵循pluginapi，不发额外opaque structure。
- groups buckets 的 window固定 `five-hour` / `weekly`（仅已知cap>0的实际窗口），remainingFraction=clamp(1-used/cap,0,1)，resetTime来自毫秒时间字段；缺失窗口不伪造0。
- summary是 credits API明确给出的monthly_credits/purchased_credits/free_credits和同账号这些组件的total_credits（单个账号内相加，不是key池求和）；unit=credits，format=number，不猜USD。不拿monthly余额当cap，不生成假monthly进度条。
- subscriptions 上游提供 planID/status/period 数据；CPA v8.0.23 的 QuotaSubscription 没有 Status/period 字段，因此只映射真实 planID，不把 active 等状态伪装为 TierName，不硬编码 allowance。

UI 中文、无框架/CDN/浏览器持久密钥、纯文本截断+title、支持proxy prefix、导入/删除/按点击quota、模式和catalog只读状态。可保留手动模型editor，但必须走既有CPA保存路径且失败不清空输入。不轮询、不直接调上游、不做收费连通性推理测试。

## 4. 验证与发布

make test = go vet/test + node UI tests；race；native c-shared build；真实CPA v8.0.23本地mock三协议两mode/stream矩阵、wire order、quota、401/402/403/422/429/截断/取消/多key亲和。

初始GitHub仓库空，已本地first commit。所有开发提交在worktree分支，主Agent Review/集成。Coder不push/tag/release，不操作main，不改其它repo或系统认证环境。
