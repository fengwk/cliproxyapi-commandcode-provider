# 当前实现契约

本文描述插件当前公共行为。`docs/design.md` 只保留研究背景，不覆盖本文。生产代码只用公共 CPA v8.0.23 SDK，不用 CPA internal。

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

- ProviderID `commandcode`，PluginID `cliproxyapi-commandcode-provider`，展示名 CommandCode Provider，版本变量 `provider.Version`。
- YAML `mode` 只是缺省默认（`cli` 或 `provider`，缺省 `cli`）。它只用于没有根级 `mode` 的旧凭据，以及未传 `mode` 的导入。已持久化的显式 mode 不受该默认值改变；改 YAML 不会把全部凭据切到同一模式。其余配置：base-url(API authority), zdr(false), models(白名单), manual-models。
- 物理 auth JSON 可带根级 `mode`：`cli` 或 `provider`。这是人工配置，不检测套餐，也不在 403 或其他错误后切换。`model.for_auth` 与 execute 共用严格解析：最新 AuthMetadata > AuthAttributes > StorageJSON > 插件默认。显式存在但类型错误、空值或非法值失败关闭，不向后续来源降级；缺少字段才用下一来源。非法模式不发上游。
- parseAuth 校验可选 mode，并保留到非秘密 Metadata；不把 mode 写入不可变 Attributes。executor 在 lookupFacts 前把所选模式绑定到当前请求的 config 副本。异步执行和 pause_turn 续跑使用该快照，不改共享配置。
- manual-models 使用 `{id, protocol?, context-window?, max-output-tokens?, input-modalities?, reasoning-efforts?}`；protocol 是 openai/claude/openai-response。结构、字段和枚举严格校验，且不受默认模式限制。资格随当前凭据模式变化，publishedModels 与 lookupFacts 必须一致：Provider 缺少可证明 endpoint（手动 protocol 或当前目录的 chat endpoint）则不暴露、不执行；CLI 中显式声明非 text/image 输入的手动条目不暴露、不执行。CLI wire 本身不由 protocol 改变。
- CLI 凭据发布嵌入 CLI/public 交集，默认 87 个，白名单和合格手动模型会改变数量；其余不自动暴露。Provider 凭据即使插件默认是 cli，也触发匿名公开目录刷新（不发送凭据、不选 key）并原子替换；失败保留 last-good 并标记 stale，不回退为空目录或 CLI。系统决策模型不能注册成聊天模型。`model.static` 继续提供默认模式目录作兼容展示，不参与凭据模式判定，也不扩大该凭据的模型集。
- auth 导入/list/native parse/prepare/refresh 只读取请求选中的 auth，不自己选 key。CPA 负责按模型选凭据。
- 只在所属 namespace 的 before-auth hook 将 x-commandcode-session 适配为 CPA affinity。UUIDv5 固定 namespace+JSON([scheme,selectedAuthID,canonicalSessionID])；header=threadID。模型 alias 仍由 CPA 管理。
- CLI 凭据：原始请求先 CLI Validate，然后公共 SDK → Anthropic 输入，再 BuildRequest；CLI Response 的 canonical Chat → 所需客户端协议，非流式与流式均经公开 SDK。执行前校验双向 transformer，防止双重转换。
- Provider 凭据首选客户端原格式的 declared endpoint，否则采用确定性的可转换路由；HTTP/NDJSON 错误不做 CLI/provider fallback。
- 流式 HTTP open + 有界读到首个可输出 frame/terminal 前置失败后才返回 RPC stream success。先存首帧而不在同步 RPC 中 emit（宿主无缓冲通道，防死锁），异步 worker再发。pause_turn 最多5次续跑，使用同一 body/auth/session/config 快照；不失败重试。
- 所有执行（含非流式与 preflight）使用 host operation_open，HTTP request带 operation_id，管理当前请求资源；cancel/停用/shutdown 可以中断阻塞 read/open。RPC error只保 HTTPStatus，RetryAfter/Retryable 与异步错误状态丢失不能假装已解决。
- 身份/配置/hash元数据不记录 key；error固定码/去敏限长；禁止用 raw upstream body 拼日志。

## 3. 管理 UI 与 quota 的接口

插件注册 resource `/ui`、`/ui.js`、`/ui.css`，资源无密钥，同源 CSP。

管理 API prefix `/v0/management/plugins/cliproxyapi-commandcode-provider`。均使用 CPA 既有管理鉴权，不增加公共写入口。读写凭据只用公开 `host.auth.get` / `host.auth.save`，不直接读写 auth-dir。

- `GET /keys` → `{files:[{name,auth_index,label,status,disabled,unavailable,success,failed,mode,mode_source,mode_error}]}`。`mode_source` 为 `auth` 或 `default`。无法确定时 `mode=""`、`mode_source=""`、`mode_error=true`，不伪装成默认 cli。单条读取失败不阻断其他条目，且不返回 raw JSON、key 或 path。只统计本 provider。
- `POST /keys` body `{keys:[...], label?:"...", mode?:"cli"|"provider"}` → `{imported,skipped,failed}`。缺省 mode 使用插件默认；显式非法、空 mode、未知字段或尾随 JSON 必须在零写入前拒绝。新凭据显式持久化 mode。重复导入仍 skipped，不改既有凭据模式。先校验/去重，最多 100 keys / 64KiB / 4096 字符；部分失败用 207；列表失败时零写入。与 PATCH 共用 importMu 串行。
- `PATCH /keys` body 只能是 `{auth_index, mode}`，mode 必须显式为 `cli` 或 `provider`；严格两个字段、无尾随 JSON、≤64KiB。先 AuthList 确认 auth_index 属于本 provider，再 `host.auth.get`；核对 name/index 与原始 provider/type，文件名只能是安全 flat `.json`。读改写只改 mode，保留 api_key、disabled、label、prefix、proxy_url、weight 及全部未知字段和精确 JSON 数字。成功 200：`{auth_index, mode, mode_source:"auth"}`。非法 400；未知或 foreign auth 404；读取或保存失败 502。消息固定，不回显凭据。宿主注册是异步的：保存成功只表示已提交，必须重新 GET 确认该条 mode 与 `mode_source=auth`，不能把“已提交”当成即时生效。
- `GET /settings` → `{mode, default_mode, mode_scope:"auth", base_url, zdr, cli_version, catalog:{count,source,fetched_at,stale}, catalogs:{cli:<catalogInfo>, provider:<catalogInfo>}, "manual-models":[...]}`。`mode` 保留给旧客户端，语义与 `default_mode` 相同，都是旧凭据默认值，不是全部凭据的模式。`catalog` 对应该默认值；`catalogs` 分别给出两种模式的目录状态。
- `POST /validate` → 检查只含 manual-models 的 patch，不保存 config；config 保存使用 CPA 已有插件 config API。
- 删除通过 CPA 认证管理 API，绝无 host.auth.delete；frontend 可以复用既有 credential delete。
- quota走CPA原生插件 `/quota` API，按 `{auth_index}` 触发；使用兼容 v0 路径 `/v0/management/plugins/<id>/quota`，标准 v8 页面路径宿主自行注册，不在插件里重复route。
- 标准 QuotaFetchResponse 使用 `subscription`、`summary`、`groups`，JSON字段遵循pluginapi，不发额外opaque structure。
- groups buckets 按 five-hour、weekly、monthly 顺序；five-hour/weekly 只取已知 cap>0 的实际窗口，remainingFraction=clamp(1-used/cap,0,1)，resetTime来自毫秒时间字段；缺失窗口不伪造0。
- monthly（月度额度，可选）只在有可信总额时发布，remainingFraction=clamp(monthlyCredits/total,0,1)。total取：`credits.monthlyCreditsGranted`（可选字段）有限且>0 时为 max(grant, 基准)；否则用**带日期的官网套餐基准快照**（2026-10-14，来源 commandcode.ai 公开静态 plan-tiers 资源）——已知套餐用其基准，teams-pro 按有效 floor(quantity)（缺失/<1 记 1）乘每席位基准；未知套餐且无授额不发布。past_due、余额缺失、授额负数等非法、total≤0 或溢出均不发布，显式非法授额不被静态基准掩盖。description 纯文本注明 total 与来源（API 本期授额 / 官网基准及快照日期 / 团队席位），resetTime 只取真实账期 RFC3339 或 YYYY-MM-DD（UTC 00:00），不推测自然月。
- summary是 credits API明确给出的monthly_credits/purchased_credits/free_credits和同账号这些组件的total_credits（单个账号内相加，不是key池求和）；unit=credits，format=number，不猜USD。monthly_credits是剩余量不是上限：不把它当分母，也不把旧额度/充值/免费余额混入月 cap。
- subscriptions 上游提供 planID/status/quantity/currentPeriodEnd；CPA v8.0.23 的 QuotaSubscription 没有 Status/period/quantity 字段，因此只映射真实 planID，不把 active/past_due 等状态伪装为 TierName，不硬编码 allowance；status/quantity/period 只用于月度窗口计算与来源说明。官网 Web 走 /internal/billing/**，插件/CLI 走 /alpha/billing/**，公开静态形状不等于 /alpha 每账号必返回 monthlyCreditsGranted；本仓库未实测真实账号返回。

UI 中文、无框架/CDN/浏览器持久密钥、纯文本截断+title、支持 proxy prefix。导入表单有 cli/provider 下拉，默认选 CLI，并显式传 mode；该选择只作用于本次导入，不表示所有凭据的模式。key 表有“模式”列、每行下拉和“保存模式”，经 auth_index PATCH。invalid/unavailable 显示错误，不伪装成 cli。PATCH 后重新 GET 确认；错误不清空列表或其他凭据。设置区只读项改称“旧凭据默认模式”，明确每凭据可覆盖；catalog 状态仍只读。配额与手动模型保留：手动模型走既有 CPA 保存路径，失败不清空输入。安全上下文门、管理密钥仅请求头、busy/认证失败提示、同源 CSP、无密钥持久化保持不变。不轮询、不直接调上游、不做收费连通性推理测试。

## 4. 验证与发布

make test = go vet/test + node UI tests；race；native c-shared build；真实 CPA v8.0.23 本地 mock 三协议两 mode/stream 矩阵、wire order、quota、401/402/403/422/429/截断/取消/多 key 亲和。

混合模式集成覆盖不同 auth 走不同 endpoint、Provider-only 模型只选 Provider auth、CLI-only 模型只选 CLI auth、热修改及重启持久化、旧记录默认值、显式 mode 不受默认值改变、非法模式不发上游、foreign auth 禁止修改。

开发提交留在 worktree 分支。Coder 不 push/tag/release，不操作 main。
