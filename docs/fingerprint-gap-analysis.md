# CLI 1.88.5 与本 provider 请求指纹差异分析报告

- 日期：2026-11-09
- 性质：交接复核文档（面向零上下文 agent，所有结论附可复现锚点）
- 关联文档：`docs/system-prompt-customization.md`（系统提示词自定义能力调查，D9 的能力背景与版本边界）
- 结论一句话：**两侧协议形状一致（同端点、同 body 骨架、同 UA），但请求指纹不一致**。本 provider 是对 CLI **1.79.2** 线上抓包的固定复刻；当前安装的 CLI 已是 **1.88.5**，在 Content-Type、新增 header、新增 body 字段、真实环境信息等至少 6 处已发生漂移。

## 0. 被检对象与环境

| 对象 | 位置 | 版本/锚点 |
|---|---|---|
| Command Code CLI（安装产物，压缩 bundle） | `/home/fengwk/.local/lib/node_modules/command-code/dist/cli.mjs` | v1.88.5（`package.json:3`）；bundle 共 15 行，关键代码集中在第 2 行，验证用 `grep -o` 锚字符串而非行号 |
| 本 provider 仓库 | `/home/fengwk/proj/cliproxyapi-commandcode-provider` | commit `b2393fb54f2924702dc6eac4e333b8f662237776`（2026-11-06, "fix(provider): fail over on confirmed upstream credit exhaustion"） |
| provider 指纹目标版本 | `internal/catalogdata/data.go:7` | `CLIVersion = "1.79.2"`（硬编码） |

provider 的设计意图在其自身契约文档中有明确声明，见 `docs/contracts.md:25-26`：

> `BuildRequest` …合成目录 `/workspace/cpa/cpa-project`，UTC 日期，空 Git metadata … 省略顶层 mode/promptCache。
> `RequestHeaders` 固定绑定 catalogdata.CLIVersion … Content-Type 采用当前抓包的合并值。

即：指纹漂移不是实现 bug，而是"锁定 1.79.2 抓包"这一决策与上游持续演化之间的时间差问题。本报告量化这个差距。

## 1. TL;DR 差异清单

| # | 差异 | CLI 1.88.5 实际 | provider 现状 | 指纹影响 |
|---|---|---|---|---|
| D1 | 版本头 | `x-command-code-version: 1.88.5` | 硬编码 `1.79.2` | 高（单头即可区分） |
| D2 | Content-Type | `"application/json"` | `"application/json, application/json"`（1.79.2 线上怪癖） | 高 |
| D3 | 新增 header | `x-cli-surface`（仅 acp/rpc）、`x-oss-primary-provider`（env 可选） | 无 | 中 |
| D4 | body 新增顶层字段 | `mode`、`promptCache` | 明确省略（contracts.md:25） | 中 |
| D5 | config/slug/taste 来源 | 真实 cwd、真实目录名 slug、真实 git 状态、真实 OS env、taste 实际开关值 | 全部合成固定值 | 高（设计取舍） |
| D6 | 浏览器风格头 | 源码不显式发送 `accept`/`accept-language`/`sec-fetch-mode`/`accept-encoding` | 显式发送且 WireProfile 固定顺序 | 中 |
| D7 | permissionMode 映射 | `bypass→auto-accept`、`auto-accept/plan` 透传、其余 `standard` | 恒 `"standard"` | 低（仅 bypass 场景） |
| D8 | 认证形态 | apiKey → `Authorization: Bearer`；OAuth → `x-oauth-token` + `x-oauth-provider` | 恒 `Authorization: Bearer` | 条件性（取决于凭据类型） |
| D9 | `params.system` 内容 | CLI 本地拼装自身系统提示词（身份提示 + `renderEnvContext` 环境上下文 + skills 目录等 systemSections） | 原样转发 CPA 客户端传入的 `system`，不注入任何 cmd 提示词 | 高（内容级可区分，且为代理模式的固有差异） |

一致项（无需改动）：端点 `POST /alpha/generate`；`User-Agent: cli`；body 骨架 `config/memory:null/taste:null/skills:null/permissionMode/threadId/params`；`params{model,messages,tools,system,max_tokens(默认 64000),stream:true,temperature?,reasoning_effort?}`；tool_result→`tool` role、tool_use→`tool-call` block、thinking→`reasoning` 的消息映射；threadId UUID 校验；`x-cmd-zdr` opt-in；`x-cli-environment: production`；`traceparent` 格式 `00-<trace>-<span>-01`（CLI 取 OTel span，provider 随机生成，格式相同）。

## 2. 证据与定位

CLI 侧均为 minified bundle，下列"锚字符串"可直接 `grep -o` 复现；行号统一为第 2 行。

### D1 版本头

- CLI：`package.json:3` → `"version": "1.88.5"`。发送处：`buildCommandAuthHeaders` 内 `[wk.CLI_VERSION]: e.cliVersion`。
- provider：`internal/catalogdata/data.go:7` `CLIVersion = "1.79.2"`；使用处 `internal/commandcode/headers.go:52` `header.Set("x-command-code-version", catalogdata.CLIVersion)`。

### D2 Content-Type

- CLI 1.88.5 发送处（`buildCommandAuthHeaders` 开头）：

  ```js
  function buildCommandAuthHeaders(e){const n={"Content-Type":"application/json","User-Agent":kk,[wk.CLI_VERSION]:e.cliVersion,[wk.CLI_ENVIRONMENT]:e.cliEnvironment,...e.cliSurface?{[wk.CLI_SURFACE]:e.cliSurface}:{},[wk.PROJECT_SLUG]:e.projectSlug,[wk.TASTE_LEARNING]:e.tasteLearningEnabled.toString(),[wk.SESSION_ID]:e.sessionId,...e.apiKey?{Authorization:`Bearer ${e.apiKey}`}:{},...
  ```

  锚：`grep -o 'function buildCommandAuthHeaders.\{0,400\}' cli.mjs`。全文件 `"application/json, application/json"` 出现次数 = 0（`grep -c` 验证）。
- provider：`internal/commandcode/headers.go:50` `header.Set("Content-Type", "application/json, application/json")`；第 36-37 行注释自述是复刻 1.79.2 线上观测值；`docs/contracts.md:26` 确认有意保留。

### D3 新增 header（x-cli-surface / x-oss-primary-provider）

- CLI header 常量表（锚 `grep -o 'wk={OAUTH_TOKEN.\{0,400\}' cli.mjs`）：

  ```js
  wk={OAUTH_TOKEN:"x-oauth-token",OAUTH_PROVIDER:"x-oauth-provider",PROJECT_SLUG:"x-project-slug",TASTE_LEARNING:"x-taste-learning",CLI_ENVIRONMENT:"x-cli-environment",CLI_VERSION:"x-command-code-version",CLI_SURFACE:"x-cli-surface",OSS_PRIMARY_PROVIDER:"x-oss-primary-provider",SESSION_ID:"x-session-id",CMD_ZDR:"x-cmd-zdr"}
  ```

  `x-cli-surface` 仅在 `setCliSurface("acp")` / `setCliSurface("rpc")` 后出现（普通终端会话缺席）；`x-oss-primary-provider` 仅 env `OSS_PRIMARY_PROVIDER` 非空时出现。
- provider：`internal/commandcode/headers.go:17-33` 的 `commandCodeHeaderProfile` 与 `RequestHeaders` 均无这两个头（1.79.2 时尚不存在）。

### D4 body 新增顶层字段 mode / promptCache

- CLI generate body 构造（锚 `grep -o 'body:{config:u,memory:null.\{0,500\}' cli.mjs`）：

  ```js
  body:{config:u,memory:null,taste:null,skills:null,permissionMode:m,threadId:toWireThreadId(n.threadId),mode:n.mode,promptCache:n.promptCache,params:{model:n.model,messages:c,tools:d,system:...max_tokens:n.maxOutputTokens??mE,stream:!0,...temperature?,...reasoning_effort?}}
  ```

  其中 `mE=64e3`（即默认 64000，与 provider 默认一致）。调用链：`complete()` → `w={...a,...v.traceHeaders}`（a 为 auth headers 回调结果，isZdr 时叠加 `x-cmd-zdr:"1"`）→ `transport.postStream({route:uE, body, headers:w})`。
- provider：`internal/commandcode/request.go:80-88` `cliRequest` 结构体无 `mode`/`promptCache` 字段；`docs/contracts.md:25` 明确"省略顶层 mode/promptCache"（1.79.2 无此字段）。
- 注意：`mode`/`promptCache` 的值来自调用选项（CLI 内部子调用可见 `promptCache:"off"`）；**主聊天默认场景下这两个字段是否出现/取值待 wire 验证**（见 §5 未决问题）。

### D5 合成 vs 真实环境信息

- provider 合成值：`internal/commandcode/request.go:16-20`（`syntheticWorkingDir="/workspace/cpa/cpa-project"`、`syntheticProjectSlug="cpa-project"`、`syntheticEnvironment="linux"`）、`request.go:208-226`（`isGitRepo:false`、空 branch/status/commits）、`headers.go:54-55`（`x-project-slug` 合成、`x-taste-learning` 恒 `false`）。
- CLI 真实采集：config 构造含 `git rev-parse --is-inside-work-tree`、`git branch --show-current`、`git status --porcelain`、`git log --oneline`（锚 `grep -o 'git rev-parse --is-inside-work-tree'`、`grep -o 'workingDir:a,date:r,environment:s'`）；`x-project-slug` 取 `getCommandCurrentProjectDirName()`（真实目录名）；`x-taste-learning` 取实际开关 `.toString()`。
- 评注：合成化是 provider 的刻意隐私设计（插件不读宿主文件系统/git/机器身份，`request.go:13-14` 注释），但它与"指纹与真实 CLI 不可区分"的目标天然冲突，需要产品层决策而非单纯修代码。

### D6 浏览器风格头

- provider 显式发送（`headers.go:29-32,66-69`）：`accept: */*`、`accept-language: *`、`sec-fetch-mode: cors`、`accept-encoding: gzip, deflate`，并通过 `pluginapi.HTTPWireProfile`（`headers.go:71-75`，`HTTP1Only + DisableAutoCompression` + 15 头固定顺序）复刻 1.79.2 线上 header 顺序。
- CLI 1.88.5 源码中：`grep -c 'sec-fetch'` = 0，`grep -c 'accept-language'` = 0，无显式 `"accept"`/`"accept-encoding"` 设置。这些头在 1.88.5 的真实 wire 上是否存在取决于 HTTP runtime（undici）默认行为，需实测（§5）。

### D7 permissionMode 映射

- CLI：`toWirePermissionMode(e){return"bypass"===e?"auto-accept":"auto-accept"===e||"plan"===e?e:"standard"}`（锚 `grep -o 'toWirePermissionMode' cli.mjs`）。
- provider：`request.go:223` 恒 `permissionMode:"standard"`。当客户端请求 bypass 模式时，两侧 wire 值不同（CLI 会发 `auto-accept`）。

### D8 认证形态

- CLI：`...e.apiKey?{Authorization:`Bearer ${e.apiKey}`}:{},...e.ossPrimaryProvider?...`，OAuth 走 `x-oauth-token: Bearer <oauth>` + `x-oauth-provider`（wk 表前两项，另见 `buildAgentGenerationHeaders`）。
- provider：`headers.go:57` 恒 `Authorization: Bearer <key>`。若导入的凭据是 OAuth 型 token，形态与真实 CLI 不同。是否构成实际差异取决于本部署使用的凭据类型。

### D9 `params.system` 内容来源

- provider：只转译客户端内容，不注入任何 cmd 提示词。证据：`request.go:132-134` 注释 "It uses only client-provided content and no CLI system prompt"；`buildSystem`（`request.go:259-288`）仅把 Anthropic 请求的 `system` 字段映射为 text block；全 `internal/` 目录无任何系统提示词模板（`grep -ri 'You are\|renderEnv\|system prompt' internal/` 零命中）；`docs/contracts.md:25` "使用客户端的内容，不复制 CLI 系统提示词"。
- CLI：系统提示词在 CLI 本地拼装——systemSections 含身份提示词、`renderEnvContext`（用 config.workingDir/date/git 状态渲染的环境上下文模板 `kT`）、skills 目录等，再经 `n.systemSections?toWireSystem(n.systemSections):n.system` 进入 `params.system`。
- 评注：这一项**无法通过修 provider 对齐**——代理的语义内容属于发起方客户端，强行注入 cmd 提示词会破坏客户端功能。它是代理转发模式的固有指纹差异，上游若按 system 内容检测即可识别，需在产品层面接受该风险。

## 3. 风险评估

- 功能正确性：影响预期较小——1.79.2 形状的请求缺少 `mode`/`promptCache` 等 1.88.5 字段，上游大概率向后兼容（provider 目前正常工作即可作旁证）。
- 检测/风控：服务端可用 `(x-command-code-version, Content-Type 合并值, x-project-slug, 缺失的 accept* 头, body 缺字段)` 中的任意单项把 provider 流量与真实 CLI 流量分开。**D1+D2 是最高置信度的区分点。**
- 漂移趋势：CLI 持续迭代（1.79.2 → 1.88.5 已累计 6+ 处差异），锁定单次抓包的策略会随时间持续劣化，建议建立"版本升级 → 重录抓包 → 同步 catalogdata + headers.go + request.go + 测试"的流程。

## 4. 修复建议（若目标是对齐 1.88.5）

优先级从高到低：

1. **P0** `internal/catalogdata/data.go:7` CLIVersion → `1.88.5`（先抓包确认真实 wire 值）。
2. **P0** `internal/commandcode/headers.go:50` Content-Type → `"application/json"`。
3. **P1** `request.go` `cliRequest` 增加 `mode`/`promptCache`（`omitempty`）并确认主聊天默认取值后透传；同步 `docs/contracts.md:25`。
4. **P1** 重新评估 D6 四个头的取舍与 `HTTPWireProfile` 顺序（`headers.go:17-33` 需整体重排以匹配 1.88.5 wire 实测）。
5. **P2** `x-cli-surface`/`x-oss-primary-provider` 按条件发送（终端会话应缺席 surface）。
6. **P2** D5 合成 config 的隐私取舍：与"指纹一致"目标冲突，需明确决策（保持合成 / 可配置 / 放弃对齐）。
7. 同步更新 `headers_test.go`、`request_test.go` 中对旧 wire 契约的断言。

## 5. 未决问题（复核者需重点验证）

1. 主聊天（非内部子调用）默认场景下 `mode`/`promptCache` 是否出现在 1.88.5 wire 上、取值是什么（bundle 内部调用可见 `promptCache:"off"`，主链路值来自选项透传，未在源码层定案）。
2. 1.88.5 真实 wire 的完整 header 集与顺序：undici 是否自动补 `accept`/`accept-encoding`/`connection`（影响 D6 与 WireProfile 重排）。建议直接抓包一次 rather than 依赖源码推断。
3. `x-session-id` 语义：CLI 发的是 `sessionId`（会话标识），provider 用 `threadId` 填充（`headers.go:56`）。当前功能可用，但若上游对两者的关联做校验，需确认等价性。
4. 本部署实际使用的凭据类型（apiKey vs OAuth），决定 D8 是否是现实差异。

## 6. 复核操作清单（零上下文可直接执行）

```bash
# CLI 侧（全部命中第 2 行，属预期）
CLI=/home/fengwk/.local/lib/node_modules/command-code/dist/cli.mjs
grep -c 'application/json, application/json' $CLI   # 预期 0（D2）
grep -c 'sec-fetch' $CLI                             # 预期 0（D6）
grep -c 'accept-language' $CLI                       # 预期 0（D6）
grep -o 'wk={OAUTH_TOKEN.\{0,400\}' $CLI             # D3 header 表
grep -o 'function buildCommandAuthHeaders.\{0,500\}' $CLI   # D1/D2/D5 头构造
grep -o 'body:{config:u,memory:null.\{0,500\}' $CLI  # D4 body 构造
grep -o 'toWirePermissionMode(e){.\{0,120\}' $CLI    # D7
grep -n '"version"' /home/fengwk/.local/lib/node_modules/command-code/package.json  # D1

# provider 侧
cd /home/fengwk/proj/cliproxyapi-commandcode-provider
git rev-parse HEAD                                   # 预期 b2393fb5...
sed -n '14,33p;40,77p' internal/commandcode/headers.go   # 合成值/头 profile/RequestHeaders
sed -n '13,20p;80,88p;208,226p' internal/commandcode/request.go  # 合成 config/cliRequest
sed -n '5,9p' internal/catalogdata/data.go           # CLIVersion
sed -n '24,27p' docs/contracts.md                    # 设计意图自述

# 回归（改动后必须全绿）
go test ./internal/commandcode/ ./internal/provider/
```
