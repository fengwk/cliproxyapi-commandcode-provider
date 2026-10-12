# CommandCode CPA v8 原生 Provider 插件设计

调研日期：2026-10-11。状态：设计与离线验证完成，**尚未实现或上线插件**。

## 1. 结论与设计约束

新建独立 Go 原生插件 `cliproxyapi-commandcode-provider`，展示名 `CommandCode Provider`，provider 与公开模型前缀统一使用 `commandcode` / `commandcode/`。

- 支持两个显式模式：`cli` 调 `/alpha/generate`；`provider` 调 `/provider/v1/*`。
- 按当前 Go 套餐需求，技术配置默认 `cli`；模式切换必须由管理员操作，不做自动降级。
- CPA 负责凭据池、会话识别、亲和、冷却、重试、故障转移；插件不建立第二套调度器或会话表。
- 只通过宿主 HTTP、auth、stream 回调交互；不修改或 fork CPA，不直接连上游绕开宿主代理和审计。
- 复用 CPA 公共协议转换 SDK，自己实现 CommandCode CLI 的请求封装和 NDJSON 解析。
- 单插件实例只有一个有效 mode；v1 不做混合模式池，不允许故障转移偷偷改变计费/权限面。
- 不运行上游返回的工具，不扫描服务器工作区，不读取本机 CommandCode 凭据。
- 成本依赖严格的 token 归一化与 Observer 配套适配，不能仅靠响应 metadata。

### 上线前置风险

官方文档明确 Go 套餐没有 Provider API 权限，调用会返回 `403 upgrade_required`；但公开 Headless 自动化能力**不等于**允许重新实现 CLI 私有 endpoint。

当前服务条款明确禁止逆向工程、未经授权的衍生作品、转售/为第三方提供服务，以及一个人控制多个账户绕过套餐限制。因此：

1. CLI transport 的技术设计不能被表述为“官方支持的第三方 API”。
2. 面向公开发布或长期使用，应先确认上游授权/条款适用性；未确认时优先官方 Provider API 或官方 Headless。
3. 密钥池只承载有权使用的凭据；同账号多 key 不代表多份额度，不用于多账号促销套利或绕限。
4. 不复制分发官方 CLI 源码/系统提示词。MIT 参考实现若复制代码，必须保留其版权与许可。

此处是工程风险提示，不是法律意见；不因这些限制而把现有 Go 套餐错误地描述为可调用 Provider API。

## 2. 调研基线与证据强度

| 项目 | 当前核实结果 |
| --- | --- |
| 本机入口 | `/home/fengwk/.local/bin/cmd` → npm `command-code/dist/index.mjs` |
| CLI 版本 | `1.79.2`，隔离环境实跑 `--version` / `--help` |
| CLI 主 bundle SHA256 | `d976c2ad89f93a2b81af4d150dca99aeedfd679f15e8b8c032314e61e484da7b` |
| 本机 Node / Go | Node `v22.23.3`，Go `go1.26.1` |
| CPA 核查版本 | `github.com/router-for-me/CLIProxyAPI/v8@v8.0.23` |
| 原生接口 | ABI 1，Schema 6；C ABI 共享库，不是 Go `plugin` 包 |
| CLI 内联目录 | 95 个唯一 ID，包含未出现在公开 Provider 目录中的条目/别名 |
| 匿名公开 `/provider/v1/models` | 87 个 ID；11 个 `/messages`，68 个 Chat+Responses，8 个仅 Chat |
| 真实推理请求 | **0**；未读取或使用真实账号凭据 |
| 离线 CLI 验证 | 5 个场景，全部断言成立；正常 4 个退出 0，402 场景退出 10 |
| CPA accounting 证明 | 官方 v8.0.23 的真实解析函数，离线 Go 测试通过 |

证据分三类：

- **官方公开资料**：权限、公开 endpoint、模型路由、ZDR、条款。
- **本机源码与离线抓包**：当前 CLI 的客户端构造与消费行为。
- **待上线验证**：私有 endpoint 实际接受条件、真实上游 usage、缓存效果、扣费和 quota 最新响应。

Mock 的 usage、错误、响应 Content-Type 都是人工构造。它们证明 CLI 的解析行为，**不证明真实后端今天一定返回这些值或完全相同的响应形状**。

## 3. 参考项目：复用什么，不复用什么

| 项目 | 可借鉴 | 不直接照搬 |
| --- | --- | --- |
| `cliproxyapi-opencode-provider` | ABI shim、宿主 bridge、auth 导入、公开 SDK 转换、亲和拦截、QuotaProvider、中文 UI、CI/发布结构 | OpenCode endpoint、头、会话格式和专用模型规则 |
| `command-code-adapter` | CLI body/message/tool 映射、NDJSON→三协议的处理思路、gzip/deflate、账号查询 | 内部 key scheduler/session registry；独立推进的 CLI 版本与模型缓存；随机仿真 Git；旧头顺序 |
| `commandcode-go-cliproxyapi` | Go 原生 ABI、Provider catalog、账号配额结构、MIT 实现参考 | CPA v7 依赖；“Responses 路由不存在”的旧假设；固定套餐额度当最新事实 |
| `pi-commandcode-provider` | CLI 能力快照生成、reasoning/modalities 补充、生成协议映射 | `403 upgrade_required` 自动回退；进程内 transport/key 状态；目前目录快照版本 `1.56.0` |

旧 Go 项目注释所称 `/responses` 不存在，已被当前官方文档与目录中的 `supported_endpoints` 否定。

## 4. 架构与职责分界

```text
OpenAI Chat / Anthropic Messages / OpenAI Responses 客户端
                         |
              CPA 会话识别 + 亲和 + 选凭据
                         |
         CommandCode 插件：请求级 prepare / model route
                  /                      \
               cli                    provider
      统一 CLI message 表示       公共 SDK → 上游 native 协议
      合成 config + CLI headers   supported_endpoints 决定路由
      /alpha/generate NDJSON      /provider/v1/* JSON/SSE
                  \                      /
                 请求级响应归一化 / 三协议输出
                         |
                 CPA usage / 错误 / 冷却
                         |
         Observer：独立的 CommandCode accounting 兼容
```

插件允许的状态只有配置/catalog 快照、正在执行的请求与流的资源句柄、必要的导入互斥锁。没有 `session → key` 表、配额调度、跨请求重试计数器或“账户最优选择”。

## 5. 配置与模式选择

以下是**拟议配置**，不是现有插件可用配置：

```yaml
routing:
  strategy: round-robin
  session-affinity: true
plugins:
  enabled: true
  dir: "./plugins"
  configs:
    cliproxyapi-commandcode-provider:
      enabled: true
      mode: cli
      base-url: "https://api.commandcode.ai"
      zdr: false
```

最小配置面：

| 字段 | 语义 |
| --- | --- |
| `mode` | `cli` 或 `provider`，默认 `cli`，不接受 `auto` |
| `base-url` | API authority，不包含 `/alpha` 或 `/provider/v1`；固定拼接路径 |
| `zdr` | 默认 false，匹配原 CLI 的 opt-in；true 必须发送 `x-cmd-zdr: 1`，不静默降级 |
| `models` | 可选模型白名单，不创造目录外路由 |
| `manual-models` | 显式新增/覆盖模型 metadata；provider 模式必须声明受支持 endpoint |

不独立提供 `cli-version` 随意覆盖：CLI 版本、wire profile、能力目录属于同一个兼容快照。版本应在管理页只读展示，避免“头是最新版、实现仍是旧版”。

配置校验禁止 URL 中的 userinfo、query、fragment；生产 HTTPS；本地 HTTP mock 仅在显式测试配置允许。凭据调用不跟随到其他 authority 的重定向；不接受客户端传入上游 URL。重定向策略须在真实宿主集成时验证，不能假设 host 默认行为已经足够安全。

热重载采用不可变快照：新请求使用新 mode，进行中的请求完整保留原 mode/config。无效配置保留旧快照，不产生半切换状态。

## 6. 凭据与 CPA 原生管理

API key 是首版唯一认证方式，不做浏览器 OAuth 登录流程。

- 注册 `AuthProvider(commandcode)`，导入后通过 `host.auth.save` 保存至 CPA auth-dir。
- 每次执行只使用 CPA 已选择的 `AuthID` 对应凭据；不自己 list 后选择另一把 key。
- 导入去重并串行化；列表只返回 host auth_index、文件名、标签、启用/冷却状态，不返回 key。
- 查询使用 `host.auth.list/get/get_runtime`。当前接口**没有 `host.auth.delete`**。
- 删除走 CPA 已有认证文件管理 endpoint；禁用走宿主支持的 auth 管理路径，不虚构回调。
- 不自动读取 `~/.commandcode/auth.json`、用户 shell 环境或其它代理的 credential store。
- 不把 key 放入 YAML、URL、浏览器 localStorage、日志、错误正文或外部遥测。
- 不自动调用 `/alpha/lifecycle-events`、`/alpha/fingerprint/record`、npm 更新检查；这些不是执行模型请求的必要职责。

模式是全局的，导入 key 不做套餐推理探测；Provider 模式遇到 `upgrade_required` 明确显示需要升级，不自动改 mode。

多个同账号 key 的额度不能相加；账号耗尽时换同账号 key 也不能恢复额度。CPA 的 retry/cooldown 机制保留，但不将池化当作绕过上游账户限制的功能。

## 7. 会话、项目身份与缓存

### 7.1 会话来源

1. 上游身份的唯一权威输入是 CPA metadata 的 `canonical_session_id`。
2. 插件仅对自己的模型，将显式 `x-commandcode-session` 适配为 `X-Session-Affinity`；已有亲和头优先保留。
3. 无显式 session 时由 CPA 识别；插件不再对完整 body/prompt 计算会话身份。
4. 若宿主最终没有 canonical session，CLI 模式在上游调用前返回明确错误，不能退化为每次随机 ID。

本机 CLI 的 `x-session-id` 与 body `threadId` 相同，均为 UUID；两次独立进程生成不同 UUID。`sess_<hex>` 是另外的遥测身份，不能拿来做 generate 的 threadId。

### 7.2 无状态映射

```text
scope = JSON(["commandcode-session-v1", selectedAuthID, canonicalSessionID])
uuid  = UUIDv5(固定的插件 namespace UUID, scope)
```

使用标准 UUIDv5 的 SHA-1 命名算法，仅作身份映射、不是密码学安全保证。固定 namespace 随代码锁定，不能重启时生成。

- 同一 auth + 同一 CPA 会话：稳定、跨重启不变。
- 不同 auth 或不同会话：隔离。
- header `x-session-id` = body `threadId` = 此 UUID。
- CPA 切换凭据后上游 thread 随之隔离；不为了缓存跨账号复用 thread。

### 7.3 合成 workspace

使用最小、固定、显式的虚拟项目信息，例如 `/workspace/cpa/cpa-project` / `cpa-project`。

- 不探测真实 OS 用户名、工作目录、Git、文件内容、机器指纹。
- `structure: []`、`isGitRepo: false`、Git 字段为空；不编造提交历史或随机 dirty 状态。
- `x-taste-learning: false`，`memory/taste/skills: null`，不启用跨调用的 taste 学习。
- `date` 是当前请求 UTC 日期；这是必要的可变环境字段，跨日可能改变上游拼装的 prompt，不能保证无影响。

合成路径仅作为字符串发送，不应在宿主上创建或读取。若以后开放 workspace metadata，必须由管理员显式提供，仍不主动读取文件。

### 7.4 缓存策略

保持会话、system、工具顺序、message 内容与 cache_control 稳定；不注入随机 Git/机器字段，不在提示词中嵌入 trace/session 随机值。

保留客户端明确提供的合法缓存标记；默认不发未核实的 `promptCache` 顶层字段。不建立第二套 prompt cache。

session affinity 只能减少换 key 和 context 波动，**不能保证**上游缓存命中率；实际缓存政策、最低 token 门槛、TTL、跨 provider 路由仍由上游决定。

## 8. CLI 模式：当前 1.79.2 wire contract

### 8.1 Transport

最低首批验证宿主为 CPA **v8.0.23**。不沿用 OpenCode 的 v8.0.15 兼容承诺。

通过 `host.http.do_stream`，传入 `wire_profile`：

```go
pluginapi.HTTPWireProfile{
    HTTP1Only:              true,
    DisableAutoCompression: true,
    HeaderProfile:          commandCodeHeaderProfile,
}
```

`http.Header` map 的插入顺序不控制线上头顺序；必须使用宿主 wire profile。RPC bridge 增加可选 `wire_profile` 与 operation_id，不仅复制旧 OpenCode 的 HTTPDoRequest。

本机 Node 22 抓到的 generate 头顺序：

```text
host
connection
content-type
User-Agent
x-command-code-version
x-cli-environment
[x-cli-surface]
x-project-slug
x-taste-learning
x-session-id
Authorization
[x-oss-primary-provider]
[x-cmd-zdr]
[x-cmd-provider-deepseek-internal]
[OAuth headers]
traceparent
accept
accept-language
sec-fetch-mode
accept-encoding
content-length
```

实测核心值：

- `User-Agent: cli`
- `x-command-code-version: 1.79.2`
- `x-cli-environment: production`
- `accept: */*`，`accept-language: *`，`sec-fetch-mode: cors`
- `accept-encoding: gzip, deflate`
- **当前 CLI 实测 `content-type: application/json, application/json`**，源于大小写不同的两个字段经 fetch 合并。不是旧 adapter 的单值 `Content-Type`。
- ZDR 默认不出现，显式开启才出现；traceparent 在 Authorization/ZDR 后。

1.79.2 兼容 profile 锁定这个实测表现，原样 golden test；不能假定重复 Content-Type 是服务端必需条件，也不把这项偶然实现细节推广为所有 Node/CLI 版本要求。

插件只发实际需要的字段：不伪造 OAuth token、CLI surface、internal provider switch 或硬件 fingerprint。taste 的 false 是刻意的隐私差异；其余可选字段默认省略。

因为禁用自动解压且显式声明 gzip/deflate，插件必须用标准库流式解压，处理 gzip、zlib/deflate 与受支持的原始 deflate；不凭空宣称支持 br。限制解压后大小和单行长度，防止压缩炸弹。

### 8.2 请求体

以下示例是插件拟发的最小 body；字段名已由当前源码及抓包确认：

```json
{
  "config": {
    "workingDir": "/workspace/cpa/cpa-project",
    "date": "2026-10-11",
    "environment": "linux",
    "structure": [],
    "isGitRepo": false,
    "currentBranch": "",
    "mainBranch": "",
    "gitStatus": "",
    "recentCommits": []
  },
  "memory": null,
  "taste": null,
  "skills": null,
  "permissionMode": "standard",
  "threadId": "<derived-uuid>",
  "params": {
    "model": "deepseek/deepseek-v4.1-flash",
    "messages": [
      {"role": "user", "content": [{"type": "text", "text": "Hello"}]}
    ],
    "tools": [],
    "system": [{"type": "text", "text": "<client-system>"}],
    "max_tokens": 64000,
    "stream": true,
    "reasoning_effort": "high"
  }
}
```

- `/alpha/generate` 始终流式；非流式下游也读取同一 NDJSON 并请求级聚合。
- 实际 wire 使用 `max_tokens`、`reasoning_effort`，不是 SDK 内部变量名 `maxOutputTokens` / `effort`。
- 未设置时省略 `mode` 与 `promptCache`，不是强行 null 或额外设置。
- temperature 仅客户端明确提供且模型支持时发送；max_tokens 按客户端值/兼容默认及已知 cap 校验。
- CLI `permissionMode` 的本地 `dont-ask` 在线上传成 `standard`；插件默认 `standard`，不发送 `yolo`。
- 不注入 CLI 内建系统提示词或默认 14 个工具。插件是模型 transport，使用下游 system/tools，不是完整 CLI agent。

### 8.3 Message 与工具映射

以请求级强类型表示收敛三种客户端输入，避免为三协议各写一套 CLI body。

| 输入 | CLI wire |
| --- | --- |
| system/developer | `params.system` 文本块，保留合法 cache_control |
| user text | `role: user`，`{type:"text",text}` |
| assistant text | `role: assistant`，text 块 |
| assistant reasoning | `{type:"reasoning",text}`；不能伪造签名 |
| function/tool call | `{type:"tool-call",toolCallId,toolName,input}` |
| tool output | `role: tool`，`{type:"tool-result",toolCallId,toolName,output:{type:"text",value}}` |
| function schema | `{name,description,input_schema}` |
| inline image | `{type:"image",image:"data:<mime>;base64,...",mimeType}` |

toolName 由当前历史中的 call ID 映射，不建跨请求表；重复 ID、缺失 name、无法解析的 arguments 明确报错，不改成空对象。

v1 只承诺 text、inline image、function tools 与文本 tool result。远程图片 URL、文件/音频/视频、remote MCP、自定义非 function tool、强制 tool_choice、不能无损表达的结构化输出等，本地拒绝或明确列为不支持；不能静默丢字段、读路径补数据或主动抓任意 URL。

非 vision 模型接收图片时本地拒绝，不照搬 CLI 将图片替换为占位文本的降级行为。

Responses 接受完整 input/history，v1 不伪造服务端持久化：孤立 `previous_response_id`、background、store/retrieve 等状态型能力不承诺支持。

### 8.4 NDJSON 状态机

输入按字节缓冲至完整行再 JSON decode，兼容 CRLF、跨 chunk UTF-8、尾部无换行；不把 NDJSON 当 SSE，也不逐 chunk 直接反序列化。

| 事件 | 行为 |
| --- | --- |
| `text-delta` | 立即输出 text delta |
| `reasoning-start/delta/end` | 维护当前 reasoning block，映射到三协议 |
| `tool-call` | 完整 call 转换成客户端工具调用；不自行执行 |
| `tool-result` | 上游执行的 server tool result，仅表示/透传，不二次执行 |
| `provider-metadata` | 保留当前请求内的有用用量/缓存补充，不能写入全局状态 |
| `cache-write-tokens` | 用量补充候选，不与 finish 中已包含的写缓存相加 |
| `finish` | 终止原因、最终 usage 校验、最终完成事件 |
| `error` | 解析 statusCode/isRetryable 和已知嵌套 error，转结构化错误 |
| `abort` | 中止，不伪造成功 |
| 未知扩展事件 | 可忽略；不记录内容；必须仍等到合法 terminal |
| EOF 无 terminal | `502 upstream_stream_truncated`，不能发送成功 finish |

对已识别但 malformed 的事件 fail closed；不能照搬 CLI 对所有 JSON parse error 一律跳过而漏掉关键 usage。

`rawFinishReason: pause_turn` 是协议续跑，不是失败重试：同一个逻辑请求/身份最多追加 5 次（与当前 CLI 上限一致），跨轮累计内容与各轮用量，不向下游发送中间成功结束，不调用客户端工具。若达到上限仍 pause，返回可识别的未完成状态/错误，不标记为正常 end_turn。

失败不在插件内重试。流中已输出内容后错误通过宿主 stream error 结束，不能自行换 key 重放导致重复内容。providerExecuted 的结果无法无损表达时，明确失败，不伪装成要求客户端再执行的 function。

## 9. Provider 模式

公开基准：

```text
authority:  https://api.commandcode.ai
base:       /provider/v1
models:     GET  /models
chat:       POST /chat/completions
messages:   POST /messages
responses:  POST /responses
```

每个模型的 `supported_endpoints` 是路由依据，不按名字猜“Claude 一定是某接口”、不默认全部走 Chat。

路由顺序：

1. 目标模型支持客户端原协议时直接选择同协议 endpoint。
2. 否则只选目录声明支持、且公共 SDK 已注册双向转换的确定性 endpoint。
3. 无可用映射本地 400；不把请求发给错误 endpoint 再尝试另一种模式。

Provider 与 CLI 使用同一 API key（官方文档），但同 key 不等于同权限/计费/可用性。Bearer 统一用于请求；Anthropic endpoint 增加必要的协议版本头，base path 保持 `/provider/v1/messages`。

复用 `sdk/translator` + `sdk/translator/builtin`，逐次验证 request、stream/non-stream response transformer；Responses 的 SDK `codex` 别名按已验证的 OpenCode 实践适配，不能把 protocol 名直接等同 endpoint 名。

成功响应保留合法 upstream usage、rate-limit、retry-after、request-id；失败响应的 Retry-After 无法通过当前原生 ABI 进入 CPA 的重试计时（见第 13 节）。清除 hop-by-hop 与敏感响应头，不全量转发 cookies。reasoning 文本/签名能力以原协议和模型 metadata 为准。

- 跨协议把 Responses 上游转换为 Chat/Claude 前先判定终态：显式 `status` 只有 `completed`/`incomplete` 放行，`failed`/`cancelled`/其它非终态一律返回固定脱敏 502；缺失（或非字符串）`status` 的历史样本兼容放行。不把上游错误文本或 body 回显，也不把未知状态默认成 `response.completed`；原生同协议 Responses 的 200 body 原样透传。
- Provider SSE 组帧接受 LF/CRLF/单 CR 行结束并剥离流首单个 UTF-8 BOM；EOF 时先校验残余半帧（截断即 502），只有帧完整且已由非空字符串 `finish_reason`（或等价原生终态）证明完成时，才允许无 `[DONE]` 成功并为转换器补发终态。生命周期 quiesce/shutdown 中断未完成流时，首次 close 必须携带固定的中断错误。

官方 `/systemone` 是 typesafe/jev 的概率决策接口，不是 Chat/Responses：v1 排除，不误注册为可聊天模型。

## 10. 模型发现、能力与版本维护

### CLI 模式

- 发布时生成一份受审阅的兼容快照：CLI version、bundle hash、wire revision、模型/能力 metadata 原子绑定。
- 1.79.2 内联目录有 95 个 ID，但并非 95 个当前服务承诺。默认自动暴露公开目录和 CLI 快照的交集，本次为 87 个；其余 8 个见证据 JSON。
- 目录条目不等于账户授权。不可用/套餐拒绝按上游错误展示，不能将发现成功解释为全套餐可用。
- CLI-only/别名条目需要显式 manual-models 注册并验证，默认不全部宣传可用。
- 离线保留发布时最后通过验证的快照，展示版本与时间；不用“刚抓到最新 CLI 版本”覆盖旧实现的头。
- 运行时不下载和 eval npm bundle、不要求 Node。CI/维护工具可以在授权边界内更新事实 metadata；禁止执行 bundle 来提取对象。

### Provider 模式

- 通过 host HTTP 无凭据读取公开 `/provider/v1/models`；启动/CPA 发现触发更新，限频合并，不每请求 fetch。
- 成功后原子替换内存快照；失败保留同 mode 的 last-good，标为 stale，不覆盖为空或回退 CLI。
- 若以后目录要求鉴权，只能使用宿主提供的发现上下文凭据，不构建自选 key 逻辑。
- endpoint 由实时目录决定；context/modalities/reasoning 可由当前已验证的 CLI metadata 补充，未知字段保持 unknown。
- 除非上游明确提供，不虚构 maxOutputTokens、effort levels、vision 或价格。禁止将缺失价格写作 0。

如使用最近一次成功公开目录来过滤 CLI 列表，必须绑定其来源/时间，保持请求级稳定；刷新不能改变进行中的模型执行计划。

版本维护采用 CI 检查、生成候选差异、协议 fixtures/宿主测试、人工验收、显式 release。目录 metadata 可更新，不代表未知 CLI wire 变更已经兼容。不要沿用旧 Python 的 version_checker 和 model_fetcher 两套独立版本时钟。

## 11. Token、价格与 Observer：必须独立处理的边界

### 11.1 CPA 的真实契约

CPA executor 使用 `prepared.outputFormat` 解析插件返回的 payload。`ExecutorResponse.Metadata` 不参与 usage；stream chunk 没有 metadata 字段。

客户端协议 usage 必须实际写进返回的 JSON/SSE：

- OpenAI Chat：最终 usage chunk。
- Responses：`response.completed.response.usage`。
- Anthropic：message_start/message_delta 的 usage，注意输入是 uncached，缓存独立。

不能为了上报而返回错误协议的 payload，也不能另发一条 usage 记录导致双计。

### 11.2 CLI usage 的归一化

保留值的“缺失”和显式 0 区别；拒绝负数、溢出、非整数、冲突 aliases。以当前 finish `totalUsage` 为主，补充事件只填缺失、不能重复累加。

目标 canonical 桶：

```text
I = U + R + W
O = N + Q
T = I + O

U 未缓存输入，R cache read，W cache write
N 普通输出，Q reasoning；输出价格应用于 O，不再额外加 Q
```

`inputTokenDetails.noCacheTokens/cacheReadTokens/cacheWriteTokens`、`outputTokenDetails.reasoningTokens` 与 totals 应互相验证。不能从缺失 cacheWrite、TTL 或 provider metadata 无条件推测值。

如果上游省略 T，只有在已确认协议 totals 定义且各分区自洽时才能推导；冲突的 T 不覆盖成“计算值”。模糊/冲突用量保持未分类，不以补 0、减 total 或忽略冲突方式使它可定价。

例如离线合成数据 `I=10,U=3,R=5,W=2,O=4,Q=2,T=14`：

| 输出协议 | input | output | read/write | total |
| --- | --- | --- | --- | --- |
| Chat / Responses | 10 | 4 | 5 / 2 | 14 |
| Anthropic | 3（未缓存） | 4 | 5 / 2 | 宿主从独立缓存计算 14 |

pause_turn 各次 finish 分别归一化后再相加；不能把同轮 delta 累计和 terminal snapshot 再相加。

**协议边界限制**：Anthropic usage 没有可用的独立总量传递通道，CPA 还会重新计算其总量。若一条 CLI 异常用量不能无损表示，不能捏造 Anthropic 桶：非流式返回明确 accounting/upstream error；流式终止为 error，不发送伪造的成功 terminal usage。该类记录保持未定价，原始异常仅在受控诊断/请求体捕获政策允许时保留。

其他协议可无损保留 authoritative total 且 bucket 不完整时，保留 totals，不宣称 complete。实现阶段必须用实际宿主+Observer 联合测试验证错误/缺失路径，不能只靠 SDK 层测试承诺准确账单。

### 11.3 Observer v0.4.4 不能“自动就兼容”

宿主转成 `pluginapi.UsageRecord` 时，只复制扁平 token counters，丢弃已解析的 TokenBreakdown。Observer 再调用 SDK 的 provider 语义识别。

`commandcode + executorAdapter` 对 SDK 是未知 provider，结果为 `unclassified`；v0.4.4 的特殊恢复只覆盖 `opencode-go`。因此，即使插件 payload 完全正确，当前 Observer 的非零价格仍会显示未定价。

**建议配套修改 Observer，不修改 CPA：**

1. 新增精确 cohort：provider=`commandcode`，executor=`executorAdapter`，model namespace=`commandcode/`。
2. 复用已验证的 subset / independent 两候选数学恢复方法，仅接受唯一 complete 结果或完全相同的多个结果。
3. authoritative raw total、alias 一致性、reasoning≤output、所有非负与 overflow 防护保留。
4. 不扩大到所有第三方 provider，不借 `openai` 名称绕过语义识别，不对冲突历史数据猜测。
5. 先加新记录测试；如存在历史 CommandCode 记录，再单独设计版本化原子迁移。当前不存在则不为了预防增加迁移。

价格仍由 Observer 对精确公开 ID（如 `commandcode/deepseek/deepseek-v4.1-flash`）配置；模型目录缺价格就是未配置，不是免费。实际套餐优惠、限时倍数和真实信用扣费不能被固定每百万 token 的估算成本替代。

若保持 Observer v0.4.4 不变，则只能承诺显示 token 与原始总量，**不能承诺 CommandCode 非零成本可定价**。这是可选择的部署边界，不是插件 metadata 可以解决的问题。

## 12. 配额与账号信息

注册 CPA `QuotaProvider(commandcode)`，复用官方通用管理面板的插件 quota 集成，不实现另一个轮询中心。

当前客户端/参考实现存在的账号接口：

| 接口 | 用途与证据 |
| --- | --- |
| `GET /alpha/billing/credits` | 剩余 monthly/purchased/free credits；5h/weekly 窗口 |
| `GET /alpha/billing/subscriptions` | planId、status、周期终点 |
| `GET /alpha/whoami?limits=1` | 账号 identity/limits；作为可选辅助，不每请求调用 |
| `GET /alpha/usage/summary?since=<UTC>` | 消费统计，可选详情，不是首版 quota 的必要接口 |

这些不是公开 Provider 文档中保证稳定的接口。本次没有鉴权 live probe；最新响应结构是实现前须验证的风险，不能把旧参考项目的 live-probed 注释当今天的新实测。

映射规则：

- monthlyCredits 是**剩余额度**，不是当月上限；未知 allowance 只展示余额，不能伪造进度条。
- window `used/cap/resetAt` 分别映射使用量、上限、Unix 毫秒重置时间；单位以实际响应字段合同为准。
- cap=0 或字段缺失表示未知/无窗口，不当“余额为 0”。
- 不将 `windowLimits.exceeded` 强制 decode bool；参考实现见过 null/string，使用各窗口的可信字段。
- 计划表只用于有来源/版本的显示 metadata；不用旧 planAllowances 推算实际月度 cap。
- 余额为 dollar/credit 时明确单位，不将余额与 token 估算成本直接等同。
- 同账号多 key 界面显示“可能共享账户额度”，不求和。
- 按用户点击查询，限并发/短超时，不轮询、不请求自动充值、不“重置上游额度”。
- 宿主 reset cooldown 只重置 CPA 本地路由状态，绝不标成恢复供应商 quota。

## 13. 错误、冷却、取消与资源生命周期

| 情况 | 插件行为 |
| --- | --- |
| 本地参数/unsupported model | 400/明确错误，上游调用为 0 |
| 401 | 保留认证错误性质，不泄露原 key |
| 403 upgrade_required | 插件不重试、提示套餐权限；不自动切 CLI；CPA 可能按宿主策略尝试其它凭据 |
| 402 / 明确信用耗尽 | 提交内容前保留真实 HTTP 状态；交给 CPA 默认冷却/故障转移，不伪装 429 |
| 422 cmd_zdr_no_providers | 插件不重试，不撤掉 ZDR 再发 |
| 429 | 提交内容前保留状态；交给 CPA 默认 backoff，不宣称 Retry-After 被原生 ABI 传递 |
| 5xx / transport timeout | 有类型的失败，由 CPA retry budget 决定 |
| 流中 error/EOF | 明确失败，不伪造成功，不内部重放 |
| 客户端取消/插件停用 | 取消当前 host HTTP operation、关闭上下游 stream、释放 parser |

错误返回使用原生 error envelope 的 `HTTPStatus`；仅写状态到 JSON metadata 不会正确触发宿主调度。插件可以填写 envelope 的 Retryable，但 v8.0.23 的 RPC 解码器丢弃它，不能声称该标记控制了 CPA 是否换 key/重试。

### 13.1 原生 ABI 的三个已确认限制

1. `pluginabi.Error` 没有 Retry-After/错误 headers；宿主 `rpcError` 只保留 Code/message/statusCode，未实现 `RetryAfter()`。上游 429 的精确等待时间不能无损传递到 CPA。
2. RPC 解码不保留 Retryable，宿主凭状态和自身策略决定冷却/故障转移。
3. `host.stream.emit/close` 的 error 是字符串，最终成为普通 Go error，丢失 HTTP 状态/重试提示。中途 402/429 可以在下游协议 error event 表示真实 code，但 CPA 的失败记录/冷却不能保证识别该上游状态。

零 core 修改的实现策略：

- 非流式的 HTTP/NDJSON 错误通过失败 envelope 返回，HTTPStatus 保留。
- 流式在返回成功 stream handle 前做有界 preflight：读取至首个可输出内容或 terminal error；缓存当前请求内的首帧，不先提交空成功。尽量让 HTTP200+首帧错误也走失败 envelope。
- 一旦已返回流并输出内容，只发送明确的协议错误并关闭，不自行换 key 或重放；CPA 使用其能收到的通用失败信息。
- 接受 CPA 默认 backoff，quota UI 单独显示上游 resetAt，不把它伪装成已设置的 CPA 冷却时间。
- 不建立影子 cooldown 表，不借 error message/假的 HTTP 状态诱导调度，不调用 reset cooldown 来模拟设置冷却。

已查源码的默认路径：401/402/403 通常设置 30 分钟冷却（关闭 cooling 时例外）；非请求参数错误可进入下一凭据尝试。429 使用宿主 quota backoff。最终仍须 native-host 集成验证配置、首帧/中途时机和选择结果。

若要求所有原生流错误保留精确状态，以及按上游 Retry-After 精确冷却，当前 v8.0.23 ABI 与“零 core 修改”不能同时满足。需等待官方 ABI 增强或放宽该能力要求；本方案选择保留零 core 修改。

打开 HTTP 前调用 `host.http.operation_open`，带 host_callback_id；HTTP 回调带 operation_id。取消用真实存在的 **`host.http.cancel`**，关闭用 `host.http.stream_close`。operation_open 不是一个叫 operation_cancel 的虚构接口。

Downstream 通过 `host.stream.emit/close` 输出；关闭、取消和 shutdown 必须幂等。请求完成后不得遗留 goroutine、解压 reader、operation 或远端连接。

只记录固定错误码、HTTP status、duration 和不可逆非敏感身份，不将上游 arbitrary error body 拼进日志/HTML。下游需要的错误文字也须去敏、限长，不能绝对相信上游不会回显 Authorization。

## 14. 管理 UI、安全与范围

首版复用 OpenCode 管理页的原生同源嵌入方式，中文，无 CDN/前端框架：

- key 导入/去重/列表/CPA 原生删除入口。
- 模式、CLI compatibility revision、catalog 数量/更新时间/stale 状态。
- key 级 quota 查询，不自动用模型请求做连通性测试。
- 不在列表回显 key，不把管理 key 或上游 key 放浏览器持久存储。
- CSP 同源、DOM textContent、输入限长、表格 `.cell-truncate` + `title`。
- 生产 body capture 默认关闭；若使用 Observer opt-in，保留其 24h 策略。

首版不做代理配置第二套 override、OAuth 账号管理、模型价格商城、prompt 编辑器、自动升级下载器、多租户管理、多个虚拟机器身份或复杂配置 DSL。

准确 token counting 的独立接口未获确认。v1 不把字符数/通用 tokenizer 估算声称为供应商精确结果；未实现的 CountTokens 返回明确不支持，文档与测试注明。

## 15. 建议代码结构与依赖

```text
cliproxyapi-commandcode-provider/
  main.go                       C ABI exports / host callback shim
  internal/provider/
    provider.go                 IDs / capabilities / registration
    config.go                   validated immutable config
    bridge.go                   host HTTP wire/operation/auth/stream RPC
    auth.go                     import / concrete auth preparation
    models.go                   catalog / deterministic route
    session.go                  canonical session → UUID
    executor.go                 prepare + cli/provider dispatch
    cli_request.go              canonical messages → generate body
    cli_headers.go              versioned HTTPWireProfile
    cli_stream.go               bounded NDJSON event state machine
    cli_usage.go                strict per-turn usage normalization
    response.go                 client JSON/SSE serialization
    quota.go
    management.go
  internal/web/
  resources/commandcode-compatibility.json
  scripts/                      catalog maintenance / build / release
  tests/integration/            real CPA host + synthetic upstream
  registry.json / README.md / config.example.yaml
```

生产依赖优先 Go 标准库 + 精确锁定 CPA v8 SDK；沿用已有公开 transformer 所需依赖，不带 Python/Node 运行时，不导入 CPA internal 包。本次 Go proof 导入 internal **仅是隔离研究测试**，不能作为生产实现结构。

无需独立 database、cron、后台 auth scanner。CLI 能力快照为只读资源，执行时不修改；管理修改交由配置/auth 宿主管理。

## 16. 实施切片与验收

### A. ABI / config / auth / session / wire

先在隔离 git workspace 建立新仓库骨架；实现 host bridge、导入和 UUID 映射。以真实 CPA v8.0.23 接本地 HTTP/HTTPS raw capture，证明头顺序/大小写、HTTP/1.1、auth、代理和取消确实生效。

不能仅凭 httptest 的 Header map 顺序或 mock callback JSON 判定线上 wire 正确。

### B. CLI 请求与事件转换

从本次 1.79.2 捕获固化 fixtures，完成 text/reasoning/tools/inline image/usage/terminal state。支持三客户端协议及流/非流；不足的字段明确拒绝。

### C. Provider catalog / execution / quota / UI

加入 supported_endpoints 路由、动态 public catalog、权限错误与 ZDR，quota 使用无凭据 synthetic fixtures 验证形状容错。不得为了完成测试而真实调用充值/推理。

### D. Observer 独立兼容与联合验证

另一个 Observer 隔离 workspace 修改精确 cohort，审阅 token 数学和非零价格回归；不改变 CPA 核心，不随意扩大 OpenCode migration。

### E. 文档、发布与后续维护

集成通过后补兼容范围、不支持能力、风险说明、store registry、多平台 C ABI artifacts 与 checksums。**push/tag/release 均需明确授权**。

### 必须通过的验收矩阵

- 两种 mode × 三种客户端协议 × 三种上游模型家族 × stream/non-stream，基础 36 cells；不支持组合是可检查的本地拒绝，不是假成功。
- 两个或更多有权使用的 synthetic keys：同 session 命中同 key；真实 CPA 冷却后按策略切换；新 key 的上游 UUID 隔离；无插件 session map。
- Chat-only 模型从 Responses/Anthropic 客户端经已注册 SDK 转换；Claude 只能发其声明支持 endpoint。
- NDJSON 单字节切分、UTF-8/emoji 跨块、CRLF、多个事件同块、末行无 newline、gzip/deflate、unknown event、invalid JSON、超长行、截断、abort/error。
- tools 的 call ID/name、重复/并行 calls、tool results、history、invalid arguments、不支持 content 不静默丢弃。
- pause_turn 边界与累计 usage；不得隐藏无限上游调用或双计。
- 完整/缺失/显式零/矛盾/负数/溢出 tokens；cache read/write、TTL、reasoning，stream 与 non-stream 完全一致。
- 实际 host→Observer 路径非零价格可定价且算术正确；矛盾记录保持未定价；不只测试 sdk.NewSubset。
- 401/402/403/422/429/5xx、Retry-After、body 去敏、取消/停用无泄漏、重定向不泄 key。
- build/test/race/fuzz；中文 UI 的导入/失败/删除/配额/stale/无权限；Linux amd64/arm64、macOS arm64 构建与 native-load smoke。

Mock/真实宿主验证是开发阶段默认。真正后端验收必须另行获得账号与调用授权、明确预算，最小量验证 CLI/API 可用性、配额与缓存；本次没有做此类调用。

## 17. 尚未通过真实后端验证的事项

| 项目 | 已知边界 / 推荐处理 |
| --- | --- |
| 私有 endpoint 授权 | 官方支持与条款限制不能由抓包证明；上线前确认 |
| 真实 CLI 服务端行为 | 本机源码+mock 确认客户端，不能保证服务器拒绝条件与完整事件集合 |
| 合成 workspace 接受性 | 最小字段与本机 shape 一致；是否影响上游 prompt/工具行为待受控验证 |
| 特殊/隐藏模型 | 默认不宣传 CLI 95 个均当前可用，manual 后单独验证 |
| 真实 usage / TTL | 以 authoritative totals 与完整字段验算，冲突不“修好” |
| 账号 quota 最新 schema | 参考实现与当前 CLI 路径可用作线索，不能当最新鉴权实测 |
| 原生错误/冷却保真 | 状态前置错误可保留，Retry-After 与异步 stream 类型信息在当前 ABI 丢失；不承诺精确冷却 |
| 缓存命中 | 结构稳定与 session affinity 是措施，命中率与扣费不能离线保证 |
| Responses 状态型功能 | v1 不支持 previous_response_id-only/store/retrieve，避免假造服务端会话 |
| 老 CPA 兼容 | 首批最低验证 8.0.23；未测版本不做支持承诺 |

上述未知不阻止给出完整工程设计，但阻止将插件宣称为已经生产验证、全功能或被供应商授权。

## 18. 官方来源

- [Provider API](https://commandcode.ai/docs/provider)：endpoint、Go 禁止 API、supported_endpoints、terminal usage、ZDR 与错误。
- [Headless](https://commandcode.ai/docs/headless)：官方 CLI 自动化路径，不等于私有 endpoint 授权。
- [CLI Reference](https://commandcode.ai/docs/reference/cli) / [Quickstart](https://commandcode.ai/docs/quickstart)：官方参数与安装。
- [Pricing & Limits](https://commandcode.ai/docs/resources/pricing-limits)：以当前文档为准，不固化优惠承诺。
- [Go Plan](https://commandcode.ai/docs/plans/go)：套餐范围。
- [Zero Data Retention](https://commandcode.ai/docs/resources/zdr)：opt-in 及路由/价格影响。
- [Terms](https://commandcode.ai/terms)：账户、逆向工程、转售/第三方使用限制。
- [公开模型目录](https://api.commandcode.ai/provider/v1/models)：本次匿名 GET 87 个 ID，快照存于证据目录。

详细本地代码定位、抓包断言、研究测试与快照见 `EVIDENCE.md`。现有 Observer/OpenCode 插件与参考仓库均未因本调研修改。
