# CLI 请求指纹复核：当前实现、实测差异与改造边界

## 1. 结论与检查范围

**当前实现不能满足与原生 cmd 请求指纹完全一致。CPA session 复用方向正确，但 HTTP、TLS、会话 ID 版本、workspace 一致性和部分 JSON 表示仍有差异。**

本次只检查并新增报告，没有修改实现。以下两份是待复核输入，不作为已验证结论直接沿用：

- [初步指纹分析](fingerprint-gap-analysis.md)
- [系统提示词自定义调查](system-prompt-customization.md)

目标按用户补充要求理解为：保留下游消息、工具和系统提示词的语义，由 CPA 识别会话和选择凭据；插件把 CPA session 适配为稳定的原生形态上游身份，不再独立计算会话；出站请求尽可能对齐一个明确的原生 cmd 配置。

必须分清两个验收范围：

1. **固定 CLI/runtime/profile 的 generate 请求一致性**：可以逐层比较和消除差异；业务输入、日期、认证值、随机 ID 等按明确规则归一化，不能把真实差异一并忽略。
2. **所有上游可观测行为均不可区分**：还包含出口 IP、请求时序、并发、连接复用、遥测和其它端点序列。仅对齐 `/alpha/generate` 的 header/body 不能证明这一点，也不能承诺“上游绝不识别”。

### 被检版本

| 对象 | 本次实际值 |
| --- | --- |
| 仓库 HEAD | `b2393fb54f2924702dc6eac4e333b8f662237776` |
| 插件兼容快照 | `internal/catalogdata/data.go:7`，`1.79.2` |
| 本机安装的 cmd | `1.88.5`，`/home/fengwk/.local/lib/node_modules/command-code/package.json:3` |
| cmd bundle SHA-256 | `c75b34df6487de034c4419b8919432309c966505c20b76a361dea9db80cda2fc` |
| Node / OpenSSL / undici | `22.23.4` / `3.6.5` / `6.29.1` |
| Go / 仓库固定 CPA SDK | `1.26.1` / `v8.0.23` |

本报告未检查生产 CPA 二进制版本、真实密钥、真实账号配置或真实上游接受性；涉及宿主的结论针对仓库声明的 CPA v8.0.23。

## 2. 证据获取方式与原生请求

### 2.1 安全边界

- 原生 cmd 在 bubblewrap 隔离网络中执行；根文件系统只读，写入仅开放独立临时目录。
- HOME 指向临时目录，真实 `~/.commandcode` 被隐藏；仅使用 `capture-dummy-key`。
- 在 `globalThis.fetch` 入口把 generate 地址改为本地 HTTPS 接收端；保留原生传入的 headers/body 和原生 Node fetch。
- 其它 API 返回本地合成数据，没有访问真实 CommandCode 推理服务、发送真实凭据或产生推理费用。
- HTTPS 接收端保留解密后的原始 HTTP 请求行、header 大小写/顺序和 body，而不是只观察解析后的 `http.Header`。
- 改写目的地影响 Host、SNI、地址等，因此不把本地目的地字面值当成生产指纹。HTTPS 默认编码声明、header 合并和 JSON 序列化仍可直接观察。

### 2.2 成功执行的样本

1. 普通 `cmd -p`：本地 mock 连续两次返回 `pause_turn` 后结束，共 3 次 generate。
2. 同一临时 HOME/cwd 下新进程 `cmd -p --continue`：1 次 generate。
3. `cmd -p --mod <本地自写 mod> --yolo`：mod 把 system 替换为固定测试字符串，1 次 generate。

上述三个运行均正常退出，5 次请求的关键断言全部通过。打印模式与交互模式的工具集可能不同，本次不把 headless 的具体工具数量泛化为所有 cmd 会话。

### 2.3 原生 HTTPS header 顺序

以下来自上述本地实测，动态值用占位符表示：

```http
POST /alpha/generate HTTP/1.1
host: <目标 authority>
connection: keep-alive
content-type: application/json, application/json
User-Agent: cli
x-command-code-version: 1.88.5
x-cli-environment: production
x-project-slug: <slugify(完整 cwd)>
x-taste-learning: false
x-session-id: <UUIDv4>
Authorization: Bearer <测试 key>
traceparent: 00-<trace-id>-<span-id>-01
accept: */*
accept-language: *
sec-fetch-mode: cors
accept-encoding: br, gzip, deflate
content-length: <实际 body 字节数>
```

普通主聊天及本次自定义 system 路径的 body 顶层键均为：

```text
config, memory, taste, skills, permissionMode, threadId, params
```

**没有 `mode` 和 `promptCache`。** 不代表所有内部子调用都省略；源码确有带 feature mode 或 `promptCache:"off"` 的其它调用。

原生 bundle 的关键锚点均可在 `dist/cli.mjs` 中检索：

- `createNodeTransport`：先放入小写 `"content-type":"application/json"`，再合并 auth headers 中的大写 `"Content-Type":"application/json"`，最终由 fetch 合并为上述双值。
- `createModelClient`：body 写了 `mode:n.mode,promptCache:n.promptCache`，但值为 `undefined` 时被 `JSON.stringify` 省略。
- `toWirePermissionMode`：`bypass → auto-accept`；`auto-accept/plan` 原样；其它为 `standard`。
- `toWireThreadId`：合法 UUID 原样，不合法则省略；插件目前选择更严格的本地拒绝。

## 3. 当前 CPA session 适配：应保留什么、需要改什么

### 3.1 当前链路

```text
显式 x-commandcode-session（可选）
  → before-auth 适配为 X-Session-Affinity；已有 affinity 优先
  → CPA 识别 canonical_session_id，并负责凭据亲和/故障转移
  → 插件接收 selected AuthID + canonical_session_id
  → UUIDv5(固定 namespace, JSON(["commandcode-session-v1", AuthID, canonical]))
  → x-session-id = body.threadId = 此 UUID
```

证据：

- `internal/provider/session.go:25-63`：只读 CPA canonical metadata，不重新对 prompt/body 做 session 哈希。
- `internal/provider/session.go:113-128`：仅所属模型的显式头做适配，已有 affinity 不覆盖。
- `internal/provider/cli.go:48-56`：缺 canonical 时本地失败；按所选 AuthID 隔离。
- `internal/commandcode/headers.go:56`、`internal/commandcode/request.go:224`：header 与 body 同源。
- CPA `sdk/cliproxy/session/identity.go:228-279`：宿主产生/归一 canonical metadata。
- CPA `internal/pluginhost/adapters_executors.go:1050-1067,1095-1106`：合并 request/options metadata 后传给插件。

这里没有额外“注册上游 session”的 API 调用；generate 的 header/body 携带适配结果。

### 3.2 已经满足的部分

- 同一 auth + 同一 CPA session：ID 稳定、跨请求和重启可复算。
- 不同 auth/session：隔离；CPA 故障转移后得到不同上游 thread。
- 不建立第二套 session 识别器，不在插件里自己选 key。
- `x-session-id == threadId` 与本次原生样本一致，不是一个需要拆分的差异。

**不能因为原生使用随机 UUID，就改成每次请求生成新 UUID。** 原生 `--continue` 实测复用前一进程的同一 UUIDv4；一次请求里的 3 次 pause 续跑也复用 ID 和 traceparent。

### 3.3 真正的会话差异：UUIDv5 与 UUIDv4

插件 `session.go:73` 强制 version=5；原生新会话来自 `crypto.randomUUID`，实测第三段首字符为 `4`。

建议保留 CPA scope 和隔离规则，只调整 ID 的出站表示/映射：

- 要求原生随机生成语义时：以 CPA scope 为键，首次分配随机 UUIDv4 并持久保存映射。该表只适配身份，不自行识别 session。
- 若选择无状态稳定摘要并设置 v4 version/variant 位，只能称为“v4 外形的稳定 ID”，不能声称它就是原生随机生成算法。
- 迁移 ID 算法会改变已有会话的上游 thread，需要明确兼容旧会话或一次性失去原有 thread 亲和，不能静默切换。

### 3.4 缓存亲和仍需要其它条件

`config.example.yaml:24-27` 已给出 `session-affinity:true`、TTL `1h`。CPA `sdk/cliproxy/service_config.go:51-82` 证明开关决定是否安装亲和选择器。

稳定 thread 不等于稳定选 key；稳定选 key/thread 也不等于 prompt cache 必命中。还需保持 system、tools、历史消息前缀和缓存标记稳定；上游 TTL、路由与缓存策略未在本次生产验证。

## 4. 已确认差异与调整位置

### 4.1 HTTP 传输：现有 profile 并没有复刻完整 header

插件 profile（`internal/commandcode/headers.go:14-33`）故意不列 `host` 和 `content-length`。

CPA 实际实现 `internal/httpwire/ordered_conn.go:140-165` 是：

1. 大小写不敏感匹配 profile 中的头。
2. 按 profile 给出的名称大小写和顺序写出。
3. **未列出的头统一追加到已列头之后。**

所以不是“Host/Content-Length 仍由宿主放在原位置”，而是最终 `Host` 被移到末尾，并保留 Go 的 `Host` / `Content-Length` 大小写。profile 能包含这两个名字；由宿主管理值不等于不能指定它们的位置/大小写。

此外 CPA `internal/pluginhost/http_bridge.go:506-510` 每次关闭 idle 连接并设置 `DisableKeepAlives=true`。Go `net/http/transport.go:2866-2869` 因此额外写入 `Connection: close`；插件自身又显式写了 keep-alive。

使用仓库真实 `RequestHeaders` / `BuildRequest`、CPA 同版本 `httpwire` 实现和同样的 stdlib transport 配置，在本地 raw TCP 复现出：

```http
POST /alpha/generate HTTP/1.1
connection: keep-alive
connection: close
content-type: application/json, application/json
User-Agent: cli
...其它 profile 头...
accept-encoding: gzip, deflate
Host: <目标 authority>
Content-Length: <body 字节数>
```

这是源码对应的最小复现，不冒充已运行生产 CPA 的抓包。与原生样本的差异包括重复且矛盾的 connection、Host 的位置/大小写、Content-Length 大小写和不复用连接。原生 pause 续跑的 3 次请求实测使用同一 TCP/TLS 连接。

**调整：** 补全严格 header profile；同时解决宿主 transport 的 keep-alive/生命周期问题，不能只把 `Connection` 头写得更像。当前 SDK wire profile 没有连接池策略字段。

### 4.2 TLS：当前 HTTPWireProfile 不具备 Node 指纹能力

CPA 默认插件路径走 `crypto/tls`（`internal/pluginhost/http_bridge.go:486-502`），profile 只支持 HTTP1Only、DisableAutoCompression 和 HeaderProfile（`sdk/pluginapi/types.go:901-909`）。

同环境额外做了 loopback ClientHello 对比：原生 Node fetch 与按宿主配置构造的 Go transport 均只协商 HTTP/1.1，但：

- Node 提供 52 个 cipher suites，Go 提供 13 个；顺序也不同。
- TLS extension 集合/顺序、supported groups 不同。
- 例如 Node 的 TLS 1.3 cipher 列表开头为 `4866,4867,4865`；Go 整体列表从 `49195,49199,...` 开始，TLS 1.3 suites 位于后面。

该测试只比较本机 runtime 默认 ClientHello，不是生产端点 TLS 抓包；已足以否定“HTTP1Only + header 顺序就等于原生 Node TLS”的假设。

**调整：** 严格对齐需要同版本 Node/OpenSSL 出站 transport，或者给 CPA 增加经过原生样本验证的 TLS profile 与连接池控制。不要复用其它产品的 Node/Claude Code TLS 模板来宣称已经匹配 cmd。

### 4.3 压缩能力：HTTPS 原生声明支持 Brotli

- 原生 HTTPS 实测：`accept-encoding: br, gzip, deflate`。
- 插件 `headers.go:69`：`gzip, deflate`。
- 插件 `decoder.go:70-98`：只有 identity/gzip/deflate，没有 Brotli 解码。

**调整：** 同步请求声明和响应解码；只给头增加 `br` 会让合法上游响应解码失败。这里不能仅依据 CLI 版本推断变化，HTTP/HTTPS 目的地及 Node runtime 都影响默认头，应以 HTTPS 基线验收。

### 4.4 版本快照

`catalogdata/data.go:7` 固定 `1.79.2`，本次原生为 `1.88.5`。这是当前版本比较中的明确差异，但旧版本号本身不是“所有原生 CLI 都不可能发送”的证据。

**调整：** CLI version、runtime、wire profile、能力目录和 golden fixtures 一起升级/锁定，不能只把头改为最新版。

### 4.5 workspace：主要问题是自洽性，不是必须读取宿主文件

原生算法：

```text
getProjectDirName({cwd}) → slugify(完整 cwd)
buildServerConfig → cwd + UTC 日期 + runtime.platform + 目录结构/Git 状态
```

插件 `request.go:16-20,208-219` 给出 `/workspace/cpa/cpa-project`，但 `headers.go:54` 的 slug 是 `cpa-project`。按原生算法，同一路径应对应 **`workspace-cpa-cpa-project`**。

这比“路径不是用户真实路径”更具体：header 与 body 在原生算法下不匹配。

非 Git、空结构、Linux、taste=false、UTC 日期都是合法原生配置，本次空临时工作区也确实得到这些值。**合成环境不必然构成协议差异，也不必为对齐而扫描 CPA 服务器的工作目录。** 可先选择一套稳定、自洽的虚拟 workspace，并用相同环境的原生样本验收；若要代表真实下游项目，则需要下游提供明确 metadata，不能用 CPA cwd 冒充客户端 cwd。

另一个缓存相关差别：原生 `createModelClient` 的 `resolveConfig` 缓存 config；插件每次 `BuildRequest` 重新计算 UTC 日期。跨日行为需要单独定义，避免同一 session 无业务原因改变环境前缀。

### 4.6 JSON 与 system 表示

| 项目 | 原生代码/实测 | 插件当前行为 |
| --- | --- | --- |
| 整段替换 system | mod 返回固定字符串后，`params.system` 为 JSON string | `buildSystem` 总是输出 text block 数组 |
| structured system | `toWireSystem` 在除末段外的 text 尾部增加 `\n`，按 sections 的 cache 标记生成 ephemeral | 原样保留下游 block 文本和 cache_control，不执行原生 sections 拼装 |
| 空 text | `toWireMessages` 保留 `"text":""` | `cliBlock.Text` 的 `omitempty` 产生 `{"type":"text"}` |
| message cache_control | 原生 `toWireMessages` 重建块时不复制此字段 | 插件会复制到 text/reasoning/tool-call/image/tool-result 等块 |
| JSON 文本编码 | `JSON.stringify` | `json.Marshal` 默认 HTML 转义 `<>&`，并转义 U+2028/U+2029 |
| 默认 max_tokens | `maxOutputTokens ?? 64000` | 默认还按模型 MaxOutputTokens 下压；显式超 cap 拒绝 |

证据：原生锚点 `toWireMessages` / `toWireSystem` / `createModelClient`；插件 `request.go:34-44,228,235-257,259-287,345-394`。

最小复现输入包含 `<>&`、U+2028 和空 text，插件实际输出了 `\u003c\u003e\u0026\u2028` 与缺失 text 的块。JSON 转义差异不改变服务端解析后的文本，不应据此断言模型缓存一定失效，但在严格 raw-wire 比较中确实不同。

**调整：** 区分 raw replacement system 与 structured sections 两条路径；校准空值、尾换行、cache_control 的范围和 JSON 编码。不能为了外观盲目删下游缓存标记或添加系统提示词，破坏下游语义。若语义与目标原生 profile 不能同时保持，必须明确该输入不属于完全对齐的支持范围。

## 5. 对两份初稿的纠正

| 初稿判断 | 复核结论 |
| --- | --- |
| D2：1.88.5 wire 为单一 application/json，应改插件 | **错误。** 只看了 auth helper，漏掉 transport 小写头与 fetch 的运行时合并；实测仍是双值，应保留。 |
| D6：源码不写浏览器头，可能已不存在 | 四个头仍存在。真正已验证的值差异是 HTTPS `accept-encoding` 包含 `br`。 |
| D4：增加 mode/promptCache 是普通请求的修复项 | 普通主聊天实测均省略；不要凭 body 字面量存在就补字段。feature 子调用另行建 profile。 |
| D3：缺少两个可选头就是普通终端指纹差异 | 普通样本没有 surface/OSS 头，当前省略与该 profile 一致；不要无条件补。 |
| D5：slug 来自真实目录名，合成环境必然冲突 | slug 来源是完整 cwd。当前确定问题是 path/slug 不自洽；可用稳定虚拟环境对齐合法原生 profile。 |
| taste=false、permissionMode=standard 总是差异 | 二者与本次普通原生配置一致。yolo/plan/taste 开启是条件性 profile 差异，不能按所有请求评级。 |
| sessionId 与 threadId 是否应相同未定 | 本次普通、续跑、恢复、自定义 system 样本均同值；真正差异是 v4/v5。 |
| D9：不同 system 内容可直接认定非原生、无法收窄 | 1.88.5 原生 mod 支持整段替换，任意 system 内容本身不是充分证据；当前确定还存在 string/array 表示差异。默认工具集/内容模式仍不同。 |

系统提示词报告的核心能力已用线上 [mods 文档](https://commandcode.ai/docs/mods)、安装 bundle 和假上游端到端验证。另有两处需修正：

- `runTransformSystemPrompt` 使用 `await`，**Promise 最终返回 string 也可生效**，不是“Promise 一律不修改”。
- 发生修改后丢弃的是 structured sections 元数据与其分段缓存表达，不是自动删除所有原有 skills 文本。返回原 system 加一段文本时，原文本仍在 flat string 内。

本次没有重新下载历史 npm tarball，因此不对“1.79.2 精确不支持哪些 hook”重新背书，也不以此推导 D1–D6 都已漂移。

## 6. 建议改造顺序

1. **先锁验收 profile。** 以已安装 cmd 1.88.5 + 当前 Node/OpenSSL + API key + production + terminal/print 默认为首个基线；feature mode/OAuth/ACP/RPC 不混为一套无条件头。业务内容允许自定义，但编码必须能与选定原生路径对应。
2. **明确宿主传输边界。** 若继续使用未修改的 CPA v8.0.23 HTTP bridge，仅调整请求字段无法解决强制 close 和 TLS 指纹。插件自建 Go/uTLS 出站的技术路径虽已补查，但用户已决定保持轻量，继续宿主 HTTP，保留这些差异。当前实施范围以 [轻量重构方案与工作量评估](cli-fingerprint-lightweight-plan.md) 为准；[自建出站调查](cli-fingerprint-plugin-plan.md) 仅留档，不再实施。
3. **保留 CPA session 作为唯一识别权威。** 保留 auth/session 隔离与 header/body 相等；调整 UUID 外形/持久映射时处理迁移，不每请求随机化。
4. **修正 path/slug 与会话级环境稳定性。** 不随机编造 Git 状态，不读取无关宿主项目来假装下游项目。
5. **校准 body 编码。** 明确 raw system/sections、空 text、cache 标记、tokens 默认值及条件字段的路径；同步必要的 Brotli 解码。
6. **升级版本快照并加严格 golden 对比。** 当前 `tests/integration/wire_test.go:426-483` 只做大小写不敏感的顺序子序列，故意忽略 Host/Content-Length 的位置，还不拒绝额外头；这解释了已有测试为何不能证明完全一致。新验收应比较完整有序头列表、重复头、完整 JSON 表示与连接行为；TLS 单独检查。

这些工作完成后，可以证明固定 profile 上已覆盖的请求面一致；业务流量、遥测/其它端点和真实缓存接受性仍需分开验收，不能用单次 HTTP golden 替代。

## 7. 本次验证结果与交付

- 原生隔离运行成功样本 5 次 generate：双值 Content-Type、HTTPS Brotli 声明、默认可选字段省略、UUIDv4、header/body 相等均通过断言。
- 3 次 pause 续跑：同一连接、同一 body、同一 session；新进程 `--continue`：session 仍相同。
- 自写 mod：system 为指定 flat string；`--yolo`：permissionMode 为 auto-accept。
- 插件真实 builder + CPA 同配置最小 wire 复现：确认双 connection、Host 在末尾、JSON 表示差异。
- 本地默认 TLS ClientHello 对比：cipher suites、extensions、groups 不同。
- 定向单元测试通过：

```sh
go test ./internal/provider \
  -run '^(TestUUIDv5KnownVector|TestSessionThreadIDStableAndIsolated|TestCanonicalSessionIDReadsMetadata|TestInterceptBeforeAuthAdaptsOnlyOwnModels)$' \
  -count=1
```

临时脚本与样本位于 `/tmp/tmpdir-12744476520936096678/`，仅用于本次复核，不作为持久项目依赖，可能由系统清理。核心脚本：`inspect-bundle.mjs`、`capture-hook.mjs`、`capture-cmd.mjs`、`provider-probe.go`、`tls-hello-probe.mjs`、`assert-captures.mjs`。未把官方系统提示词、真实凭据或完整原生 bundle复制进仓库。

生产实现未修改；没有运行全量回归、没有真实上游推理验收，也没有把 mock 缓存用量当作真实缓存命中证明。
