# CLI 请求对齐：自建出站备选方案（已排除）

> **状态：用户已选择轻量方案，继续使用宿主 HTTP，不实施本文的 uTLS、自建出站或代理管理。CLI 模式未使用，无需兼容迁移。当前完整方案与改造量评估以 [轻量重构方案](cli-fingerprint-lightweight-plan.md) 为准。** 本文仅保留此前技术可行性调查，以下推荐与待确认项不再作为实施指令。

## 1. 目标、约束与结论

用户约束：**不修改 CPA，只修改本插件；复用 CPA session 与凭据调度，尽量对齐原生 cmd 请求。**

推荐方案：**CLI 模式改用插件内的 Go/uTLS HTTP/1.1 transport；保留 CPA 的选凭据、session 识别、下游流和 usage 路径。** 不增加 Node、外部 sidecar 或第二套 key 调度器。

原因：当前宿主 `HTTPWireProfile` 不能控制 TLS 或连接池，宿主还强制 `DisableKeepAlives=true`。继续通过原有 `host.http.do_stream`，仅改头和 body 无法消除这些差异。旧约束“所有 HTTP 必须走宿主”需要在本插件契约中调整，但不改 CPA。

此文是改造计划，不是已实现功能。生产代码未修改。差异证据见 [复核报告](fingerprint-review.md)。

### 对齐范围

- 固定基线：cmd **1.88.5**、Node **22.23.4**、OpenSSL **3.6.5**、undici **6.29.1**，API key、production、普通终端/print 的 generate 请求。
- 与下游业务输入相对应的可定制 CLI 路径；保留下游 system/tools/messages 语义，不复制官方默认提示词。
- 覆盖 URL、HTTP 版本、完整有序头、JSON 表示、会话身份、TLS 握手及连接复用。
- OAuth/ACP/RPC/内部 feature 子调用不是普通请求必带项，不无条件补头或字段。
- 不承诺所有网络行为不可区分；不生成额外 lifecycle/fingerprint 遥测，不伪造硬件身份，不用无关请求“配齐”流量序列。

## 2. 插件自建出站的可行性证据

### 2.1 uTLS：本地最小验证已通过，但不是生产验收

基于已缓存的 `github.com/refraction-networking/utls v1.8.2`，在独立临时目录验证：

1. 用本机原生 Node fetch 向 loopback 接收端发送 ClientHello，包含 localhost SNI。
2. 由该样本生成 uTLS spec，重新生成密钥材料，向本地带有效测试证书的 TLS server 握手。
3. TLS 1.3 和 TLS 1.2 均完成证书验证、握手并读到 `OK`，ALPN 为 `http/1.1`。
4. 本次样本与 uTLS 发出的 **52 项 cipher suites、扩展 ID 顺序及扩展长度完全相同**；两者 TLS record payload 均为 1605 字节。

重要限制：

- uTLS 严格 Fingerprinter 报 `unsupported extension 22`。PoC 通过允许未知扩展生成了样本，**不能把 `AllowBluntMimicry` 当成生产安全保证**。
- extension 22 为 encrypt-then-MAC；生产实现需明确其在允许的 TLS 1.2 cipher 路径下的语义，不能只填原始字节。声明的其它算法/压缩扩展同样需与实际实现能力一致。
- 本次仅验证常规本地协商路径，不等于全部广告算法、真实 CommandCode 服务、HelloRetryRequest 或恢复握手已兼容。
- 不把 Node 原始随机数、session ID、key-share 公钥固化进生产 profile；profile 只定义结构与能力，密钥材料每次正常生成。

因此，TLS 对齐有可运行的纯 Go 技术路径，但需保留专项验收门槛，未通过的部分明确列为残余差异，不静默回退。

### 2.2 取消：现有 CPA API 足够建立插件自己的请求 context

v8.0.23 已有：

- `sdk/pluginapi/types.go:1075-1078,1188-1202`：`RequestLifecyclePlugin` 与含 RequestID/Outcome 的 `RequestCompletion`。
- `internal/pluginhost/rpc_schema.go:37`、`rpc_client.go:136-138,550-558`：native 插件可注册 `request_lifecycle_plugin`，接收 `request.complete`。
- `sdk/api/handlers/handlers_execution.go:105-109`、`handlers_stream.go:71-77,148-179`：取消时产生 canceled 终态。
- `internal/pluginhost/client_guard.go:46-74`：请求 context 取消时，宿主可停止等待底层 C 调用。
- `internal/pluginhost/adapters_interceptors.go:210-237`：终态通知使用 `WithoutCancel`，异步投递，避免因原请求取消而丢失通知。

请求级关联方案：

```text
after-auth hook 取得 CPA RequestID
  → 通过插件私有 header 传给 executor
  → executor 建立 RequestID → 本地 context/CancelFunc 关联
  → request.complete(canceled/failed/终态清理) 取消残留上游工作
```

- 私有 header 由 hook 覆盖，不信任客户端自填；只用于插件内部，绝不发往上游。
- after-auth 返回的 Headers 会合并进执行 opts，再通过 ExecutorRequest.Headers 到达 native 插件；普通 auth manager 和 plugin executor route 均有此通道。
- 使用非 session 类私有名称，避免参与 CPA 的 session 提取。
- **RequestID 是单次入站执行身份，canonical_session_id 是对话身份，两者不能互相替代。**
- 同一 RequestID 下 CPA 可有多个 attempt；完成/取消应终止其所有未结束工作，不按 session 取消，否则会误伤同会话的并发请求。
- 处理“completion 先于本地注册”的竞态，使用有界终态记录；收到重复终态要幂等。
- quiesce/shutdown 同时取消所有本地 context、关闭 response body 和空闲连接，worker drain 后才能释放库。
- 私有 header 缺失、不能建立生命周期关联时，不悄悄启动无法被 CPA 取消的自建推理；该入口需先通过兼容验证或本地拒绝。

源码已支持这一方案，但仍须用真实未修改 CPA + 编译后的插件验证非流式、首帧前阻塞、流中阻塞三种取消时序，不能用“停用能终止”替代“单请求取消能终止”。

## 3. 出站 transport 的改造项

建议新增 `internal/clitransport`，只负责 HTTP，不负责业务会话识别或选择凭据：

| 改造项 | 方案 |
| --- | --- |
| TLS | 固定版本 cmd ClientHello profile；uTLS HelloCustom；保留证书与 hostname 校验 |
| HTTP | HTTP/1.1；不进行 HTTP/2 协商 |
| 头序列 | 在实际明文 HTTP 写出位置控制完整顺序/大小写，包含 host 和 content-length；拒绝额外默认头、重复 Connection |
| 连接池 | 按 AuthID/凭据代际、authority、代理、profile 隔离；同一身份复用，不跨凭据共用连接或 TLS tickets |
| 池生命周期 | 有界缓存、清理 idle；配置/代理/凭据变化后淘汰相应旧池，不中断无关进行中请求 |
| 压缩 | 声明 `br, gzip, deflate` 时实现对应流式解码；继续限制解压后总量和单行大小 |
| 失败处理 | 延续现有状态码与额度耗尽映射；不增加插件选 key、请求重放、CLI/provider fallback |
| 重定向 | 保留安全边界，不能把 key 跟随到其它 authority；异常重定向不用于证明常规请求对齐 |
| stream | response.Body → 现有 NDJSON Decoder/Response；继续使用宿主下游 StreamEmit/Close 和 usage 投影 |

接入点是 `internal/provider/cli.go:290-325` 的 openTurn/closeTurn；同时替换 hostStreamReader。保留现有首帧前错误返回、pause_turn 续跑、usage 累计与三协议响应转换。

**Provider 模式先保持原样，避免把 CLI 指纹改造扩散到 Provider API。** CLI 身份下的 billing/quota 若也纳入对齐，需要补录该端点自己的原生头集合后使用相同出站 transport，不能把 generate 的全部头硬套到所有端点；未完成前列为残余请求面差异。

## 4. 代理：不能假装自动完整继承

native ExecutorRequest 没有公开的有效 request ProxyURL，也不携带可用的 Go HTTPClient 对象。自建 HTTP 不会自动获得宿主原 HTTP client 的代理行为。

可用信息：

- 当前插件已在 AuthMetadata/StorageJSON 中保留每凭据 `proxy_url`（`internal/provider/auth.go:35,40,89,125-127`），可以严格解析并使用。
- `auth.parse`、`model.static`、`model.for_auth` 的 HostConfigSummary 提供全局 ProxyURL；可以观察到配置快照，但 executor 请求本身没有这个字段，不能保证逐请求或热更新时序完全等价。
- 公开 `sdk/proxyutil.BuildDialer` 可作为 HTTP/HTTPS CONNECT、SOCKS5 的实现基础；代理失败应失败关闭，不暗中改成直连。

**建议的可保证方案：**

```text
每凭据显式 proxy_url
  > 插件明确配置的 CLI 出站代理
  > 明确 direct
```

插件配置可复用公开 proxyutil 的 direct/none、http/https/socks5/socks5h 语义；代理 URL 可能包含凭据，管理展示和日志必须脱敏。环境代理是否采用应显式定义，不能在“direct”下意外生效。

如果要求完全透明继承 CPA 每次执行的 request proxy override，仅靠当前公开 native ABI 无法保证。不要用读取宿主 YAML、访问未公开内存、替换宿主全局 transport 等方式绕过接口。

## 5. 身份和 workspace：保留 CPA 计算权威，轻量无状态适配

### 5.1 会话 ID 推荐方案

继续以 `(selected AuthID, CPA canonical_session_id)` 为 scope，不对 system/messages/tools 做内容哈希，不纳入 model。

推荐用选中凭据派生的密钥做 domain-separated HMAC，取 128 bit 并设置 UUID v4 version/variant 位，同时填入 `x-session-id` 与 `body.threadId`。

优点：

- 同一凭据和 CPA session 跨请求、跨重启稳定。
- 不同凭据/session 隔离；模型切换不改变身份。
- 没有 session 映射落盘、无限增长表或宿主 auth 文件写回。
- 出站版本位不再固定为可见的 UUIDv5。

**准确名称是“v4 格式的稳定派生 UUID”，不是原生随机 UUID 生成算法。** 若要求真实随机 UUIDv4 并跨重启保持，则需持久化 CPA scope → UUID 映射。当前宿主没有通用插件 KV，不应把会话表塞进 auth.save；这会引入插件自有数据目录与清理策略，不作为轻量默认。

升级会改变旧上游 thread：不会删除 CPA 会话或客户端对话历史，但旧 thread 亲和不延续，可能损失热缓存。需要用户接受迁移窗口。不能同时声称“保持所有旧 UUIDv5 不变”和“所有请求不再出现 UUIDv5”。

### 5.2 workspace 池推荐方案

沿用 Python adapter 的分层思路，不复制它的 `sess_...` 身份和 basename-slug 假设：

```text
AuthID/凭据身份 → 固定 home login + 固定 4～8 个项目候选
CPA canonical session + AuthID → 在该候选集中稳定选择项目
workingDir = /home/<login>/proj/<project-name>
header slug = slugify(完整 workingDir)
```

- 项目名称池可采用 64 项；候选池与算法固定版本，不在 CLI 版本升级时顺带洗牌。
- 同会话/凭据稳定，跨模型不变；不同会话允许共享 workspace，但 thread 必须独立。
- 项目名称和最终完整路径 slug 是两个值，不混称。
- 不读取宿主文件系统；默认采用合法的非 Git 空工作区，不合成提交、dirty 状态或不存在的文件列表。
- 环境日期按会话实例初始化形成有界快照；清楚定义过期/重启时重新初始化，而不是假称无状态算法能恢复第一次请求日期。
- 身份、workspace 摘要使用不同 domain，不把 session UUID 的一小段直接当 slug 索引。
- key 不等于账号，池隔离只按所选凭据做，不推断账号归属。

## 6. 请求头和 body：应改与应保留

### 必须保留

- Content-Type 的合并双值 `application/json, application/json`。
- 普通聊天默认省略 mode/promptCache。
- API key 的 Bearer 形态；默认不发 ACP/RPC surface 和 OSS provider 头。
- 默认 standard permissionMode、taste=false；不因下游客户端有“自动执行工具”就猜测它等于 cmd yolo。
- memory/taste/skills=null、params.stream=true。
- 同一逻辑请求的 pause_turn 续跑保持 body、thread、traceparent 一致。

### 必须校准

- 补齐完整头顺序及大小写，排除私有生命周期 header 和宿主/Go 意外添加的头。
- 从选定 workspace 派生 header slug，不再写固定 `cpa-project`。
- 版本、runtime profile、能力目录、fixtures 原子更新，不运行时只轮询版本头。
- 空 text 保留 `"text":""`；字符串编码处理 `<>&`、U+2028/U+2029 等 JS/Go 差异。
- 验证 tool schema、tool-call input 的键序与数字表示；不能为了外观改变大整数等业务数值。
- 保留输入来源：文本 system 可以对应原生 mod 的 flat string 路径；结构化 system 保留明确的分段和缓存语义，不默认全部压成字符串。
- 原生 structured sections 的尾换行、ephemeral 标记要按具体路径验证，不无条件给客户端原文增加换行。
- 客户端显式 cache_control（尤其 message 块和 TTL）优先保持其语义。若目标原生路径不能表达，标注为支持范围差异，不为“完全一致”静默删除。
- 核对默认 max_tokens 的模型调用层行为，再处理插件额外下压；当前只确认一个主模型样本的 64000，不把所有模型默认值拍成同一数字。

这里的目标是可定制 CLI 请求形态，不是复制默认 cmd agent 的提示词/工具集合。没有可表达的原生对应路径时，应明确边界，而不是声称所有任意下游请求均能逐字节相同。

## 7. 实施顺序与验收门槛

### 第一阶段：先打通纯 Go 出站及生命周期

只新增 transport、请求级取消关联和代理解析，先保持原有 codec：

- 验证完整 HTTP 头写出、重复 Connection 消失、同身份连续请求复用连接。
- 验证不同凭据/不同代理的连接与 TLS session cache 隔离。
- 对真实未修改 CPA 跑非流式、首帧前、流中取消；阻塞上游必须及时断开，停用/shutdown 能 drain，内部 header 不上送。
- 验证错误状态进入 CPA 原有冷却/重试路径；不重放已发送请求。

这些条件未通过，不推进“指纹已对齐”的发布。

### 第二阶段：身份、workspace、codec

- 校验同一 CPA session 稳定性、跨模型不变、跨凭据隔离、重启稳定派生。
- 验证每凭据小池、完整 cwd→slug 一致，以及不同会话共享项目时 thread 不合并。
- 按真实使用的 system/text/tool/image/history/cache 输入，做聚焦 golden 比较；不重新扩大到所有表单/协议功能。
- 实施并明确一次性身份迁移。

### 第三阶段：TLS/压缩与整体基线

- TLS 首次与恢复握手、SNI、ALPN、关键扩展和 HRR/所宣告能力专项验证。
- Brotli/gzip/deflate 合法响应均能增量解码并保持限制。
- 用原生 cmd + 本地 HTTPS 与真实 CPA + 新插件，对完整有序头和请求体进行对比；只归一明确的随机/身份/日期/目标地址字段，不把 UUID version、字段缺失、额外头等差异忽略。
- 将通过的 fixtures 与 CLI/runtime 版本锁定。真实上游可用性和缓存命中仍需另行受控验证，不用 mock usage 证明。

## 8. 本轮验证与待用户确认

已完成：

- 只读核实 CPA v8.0.23 的 native 生命周期、headers 传递、取消、proxy 和公开 API 边界。
- CPA 自带的 3 个聚焦生命周期测试通过：已取消 context 仍能投递终态、终态不阻塞响应、native RPC 注册/调用包含生命周期能力。
- 独立临时目录的 uTLS PoC 完成 TLS 1.2/1.3 本地握手，cipher 列表和扩展顺序/长度与样本相同；同时发现并记录 extension 22 的严格解析限制。
- 本轮没有修改生产代码、没有读取真实凭据、没有访问真实推理上游。

需要确认的取舍：

1. **CLI 模式由插件自建出站及显式代理管理**：推荐接受。若必须沿用宿主 HTTP 代发，则保留 TLS、Connection、连接池差异，不能满足本方案的近乎完整请求层对齐。
2. **身份采用无状态 v4 格式派生，并接受一次性上游 thread 迁移**：推荐接受。若不接受迁移，先保留 UUIDv5；若要求原生随机生成加跨重启稳定，则需另行批准插件持久化映射。

临时 PoC 文件在 `/tmp/tmpdir-12744476520936096678/utls-feasibility.{go,mjs,json}`，随系统清理，不作为项目依赖。Node 仅用于本次生成参考样本，生产方案不引入 Node。
