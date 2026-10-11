# CommandCode Provider

`cliproxyapi-commandcode-provider` 是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）v8 的**原生插件**（C ABI `c-shared` 动态库），把 CommandCode 作为 CPA 的上游 provider 接入。展示名 **CommandCode Provider**，稳定技术 ID `cliproxyapi-commandcode-provider`，provider / 公开模型前缀统一为 `commandcode` / `commandcode/`。

- 客户端协议：OpenAI Chat Completions、Anthropic Messages、OpenAI Responses，均支持流式与非流式。
- 两种显式传输模式：`cli`（默认）与 `provider`；**不做自动降级或自动切换**。
- 只读配额查询（CPA 原生 `QuotaProvider`），不轮询、不写回上游。
- 本仓库是独立插件仓库，不 fork、不修改 CPA 核心，只使用 CPA v8.0.23 的公开 SDK。

> 本文档描述**已实现的插件契约**。除特别说明外，所有上游行为均以合成的本地 mock 验证，**没有对真实 CommandCode 服务做过生产验收**（见“与真实上游的验收状态”）。安装可使用插件商店或手动构建方式。

## 职责边界

```text
客户端 (Chat / Messages / Responses)
        │
   CPA：会话识别 · 亲和 · 选凭据 · 冷却 · 重试 · 故障转移
        │
   CommandCode 插件：请求校验 · 模型路由 · 协议转换 · 请求封装
        ├── cli      : POST <base-url>/alpha/generate（NDJSON 上游）
        └── provider : POST <base-url>/provider/v1/*（按 supported_endpoints）
        │
   CPA：usage / 错误 / 冷却 / 故障转移
```

- CPA 负责凭据池、会话识别、亲和、冷却、重试与故障转移；插件**不建立第二套调度器或会话表**，每次只使用 CPA 已选定的 `AuthID`。
- 插件不做失败重试、不做影子冷却、不调用 reset cooldown 来“模拟”调度。
- 插件不执行上游返回的工具、不扫描服务器工作区、不读取本机 CommandCode 凭据、不主动抓取远程 URL。
- 持久化只走宿主回调（`auth-dir`、管理 API）；UI 无外部 CDN/框架，不在浏览器持久化密钥。

## 安装与使用

### 前置条件

- 支持原生插件的 CPA **v8**。首批验证基线为 **v8.0.23**；插件 SDK 精确固定在 `go.mod`（`github.com/router-for-me/CLIProxyAPI/v8 v8.0.23`）。
- 本地构建需要 Go 1.26+、C 编译器与 `make`；发布流程在原生 runner 构建 linux/amd64、linux/arm64、macOS/arm64。本地实际验证的平台为 linux/amd64，其他平台的构建结果以 CI 为准。

### 1. 从自定义插件源安装（推荐）

在 CPA 配置加入本仓库的 `registry.json` 源，然后在插件商店安装；商店读取**最新正式 Release**、校验 `checksums.txt` 并安装启用：

```yaml
plugins:
  enabled: true
  dir: "./plugins"
  store-sources:
    - "https://raw.githubusercontent.com/fengwk/cliproxyapi-commandcode-provider/main/registry.json"
```

`registry.json` 不锁定版本。自定义源不会替换官方源。

### 2. 手动构建或安装

```bash
make build        # 产物 dist/<goos>/<goarch>/cliproxyapi-commandcode-provider.{so,dylib}
make package VERSION=0.1.0   # 生成插件商店 ZIP（含 metadata.json 与 .zip.sha256）
```

`make package` 的归档名与 `metadata.json` 使用 `VERSION`：显式传 `VERSION=0.1.0` 时归档为 `cliproxyapi-commandcode-provider_0.1.0_<goos>_<goarch>.zip`；不传则本地构建使用 `0.1.0-dev+<short-sha>`。发布由 tag 决定版本。

把动态库放到 `<plugins.dir>/<goos>/<goarch>/cliproxyapi-commandcode-provider.{so,dylib,dll}`。注意 Linux 产物依赖 glibc，**不适用于 musl/Alpine**。

## 配置 CPA

参考 `config.example.yaml`（合并进既有配置，不要直接覆盖）。最小配置面：

```yaml
plugins:
  configs:
    cliproxyapi-commandcode-provider:
      enabled: true
      mode: cli                 # cli（默认）| provider，显式且全局
      base-url: "https://api.commandcode.ai"   # 仅 authority，不含 /alpha 或 /provider/v1
      zdr: false                # Zero Data Retention 默认关闭（opt-in）
      # models: [...]           # 可选白名单，不创造目录外路由
      # manual-models: [...]    # 显式新增/覆盖模型 metadata
```

- **`mode`**：`cli` 走 `/alpha/generate`；`provider` 走 `/provider/v1/*`。模式是每个插件实例的**单一全局选择**，只在管理页只读展示；无效配置保留旧快照，不产生半切换。
- **`base-url`** 只写 API authority，可选带一个**路径前缀**（例如反向代理下的 `/commandcode`）；**不要**附加 `/v1`、`/alpha` 或 `/provider/v1`，插件在 authority+前缀之后自行拼接版本化路径。生产必须使用 HTTPS；禁止 URL 中含 userinfo/query/fragment。
- **`zdr`** 默认 `false`（与原 CLI 一致）；`true` 时发送 `x-cmd-zdr: 1`，遇到 `422 cmd_zdr_no_providers` 原样上报，**不会撤掉 ZDR 再重试**。
- **`models`** 只是白名单，不新增目录外模型。
- **`manual-models`** 用于显式注册/覆盖模型：`{id, protocol?, context-window?, max-output-tokens?, input-modalities?, reasoning-efforts?}`，provider 模式必须声明受支持 endpoint（`openai`/`claude`/`openai-response`），上限 100 条、每条 id ≤ 200 字节。
- **宿主保留字段**：`enabled`、`priority` 和商店写入的 `store` 由 CPA 管理；插件接受但不把它们作为业务配置。`store` 内的版本、安装来源及其他元数据不影响传输模式或上游地址；其他未知顶层字段仍严格拒绝。

模式是**显式**的：插件不会因为 `403 upgrade_required` 或任何错误自动在 `cli` 与 `provider` 之间切换，也不会自动回退到某个固定模型列表。

### 商店安装后显示“未注册”

v0.1.0 遗漏了宿主保留字段 `store`，商店安装后可能在 `plugin.register` / `plugin.reconfigure` 返回 `invalid config yaml`；全局和实例开关开启也无法解决。这同时会导致插件名称、可视化配置字段和管理菜单缺失。

修复版本发布前的临时办法：先备份 CPA 配置，仅移除 `plugins.configs.cliproxyapi-commandcode-provider.store` 元数据块，保留该插件的其他配置与 `enabled: true`，保存后等待 CPA 配置重载并刷新插件列表。移除元数据会影响商店来源和版本追踪，之后应通过商店安装修复版本，让 CPA 重建对应新版本的 `store`；不要原样恢复旧版本元数据，也不要删除凭据或其他插件配置。反复安装同一个 v0.1.0 会再次写入该字段，不能解决问题。

## 导入 CommandCode 密钥

密钥经插件管理路由导入，由宿主保存到 CPA `auth-dir`（默认 `0600`），**不写入 YAML、日志、URL 或浏览器存储**。

- 管理界面（同源嵌入）：`GET /v0/resource/plugins/cliproxyapi-commandcode-provider/ui`
- 列表 / 导入：`GET|POST /v0/management/plugins/cliproxyapi-commandcode-provider/keys`，导入体 `{"keys":[...],"label":"..."}`，返回 `{imported,skipped,failed}`；重复导入按 `skipped` 处理。
- 删除：使用 **CPA 原生**认证文件管理路由 `DELETE /v8/management/credentials?name=<file>`；插件**没有** `host.auth.delete`。
- Settings（只读）：`GET /v0/management/plugins/cliproxyapi-commandcode-provider/settings`（`mode`、`base_url`、`zdr`、`cli_version`、`catalog`、`manual-models`）。

同账号的多把 key **额度不能相加**；账号耗尽时换同账号 key 也不会恢复额度。请只导入你有权使用的凭据。

## 发起请求

公开模型前缀为 `commandcode/`，例如：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer replace-with-client-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "commandcode/deepseek/deepseek-v4.1-flash",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

其中 `replace-with-client-api-key` 是**客户端访问本代理**的占位 key（`access.api-keys`），不是 CommandCode 上游 key。上游请求只看到原生模型 ID（如 `deepseek/deepseek-v4.1-flash`），不带 `commandcode/` 前缀。

## 会话亲和与合成 workspace

- 上游身份的唯一权威输入是 CPA metadata 的 `canonical_session_id`；插件仅把自己的模型上显式的 `x-commandcode-session` 适配为 CPA 亲和头，已有亲和头优先保留。插件**不对 body/prompt 做内容哈希**。
- 上游 thread 身份：`uuid = UUIDv5(固定命名空间, JSON(["commandcode-session-v1", selectedAuthID, canonicalSessionID]))`。同一 auth + 同一会话稳定、跨重启不变；换凭据后上游 thread 随之隔离。
- CLI 模式发送**合成 workspace**：`workingDir=/workspace/cpa/cpa-project`、`isGitRepo=false`、空 Git 字段、`memory/taste/skills=null`、`date` 为当前 UTC 日期、`params.stream` 恒为 `true`。插件不探测真实用户名、工作目录、Git 或文件内容，也不在宿主上创建该路径。
- session affinity 只能减少换 key 与上下文波动，**不保证**上游缓存命中率。

## 模型路由

- **CLI 模式**：默认暴露嵌入的 CLI/public 交集（87 个）；上游 wire 不因客户端协议改变，均经 `/alpha/generate` NDJSON。CLI-only/别名条目需要显式 `manual-models` 注册。
- **Provider 模式**：以实时 `GET <base-url>/provider/v1/models` 的 `supported_endpoints` 为路由依据。
  1. 模型声明了客户端原协议 → 直接用同协议 endpoint。
  2. 否则选择目录声明支持、且公共 SDK 已注册双向转换的确定性 endpoint。
  3. 无可用映射 → 本地 `400`，绝不把请求发到错误 endpoint 再“试另一种模式”。
- 目录发现失败保留 last-good 并标记 stale，**不回退为空目录或 CLI**；发现失败不伪装成成功。

## 只读配额

配额走 CPA 原生插件 quota 路由，按 `auth_index` 单次触发，不轮询、不重试、不缓存、不支持“重置上游额度”：

```
POST /v0/management/plugins/cliproxyapi-commandcode-provider/quota
Body: {"auth_index":"<来自 keys 列表的 index>"}
```

`groups` 只给出已知 `cap>0` 的真实窗口（`five-hour` / `weekly`），`remainingFraction = clamp(1 - used/cap, 0, 1)`，`resetTime` 来自毫秒时间字段；缺失窗口不伪造为 0。`summary` 只展示 credits API 明确给出的 `monthly_credits`/`purchased_credits`/`free_credits` 与同账号 `total_credits`，`unit=credits`、`format=number`，**不猜 USD**，不把月余额当上限画进度条。

宿主 reset cooldown 只重置 CPA 本地路由状态，**绝不**表示恢复供应商额度。

## 支持与不支持

**CLI 模式支持**：Text、inline image（`data:` base64，且模型支持 vision）、function tools 与文本 tool result、Chat/Messages/Responses 三协议流式与非流式、显式 `tool_choice: auto`、`n=1`、普通文本 `response_format`、`pause_turn` 协议续跑（同一逻辑请求最多追加 5 次，累计 usage）。

Provider 模式原协议直通保留上游原生字段，例如 OpenAI Chat 原协议直通允许 `n>1`；需要跨协议转换时，无损表达不了的语义仍在本地拒绝。

**CLI 模式不支持（本地明确拒绝，不静默丢弃）**：

- 远程图片 URL、文件/音频/视频、remote MCP、自定义非 function tool、强制 `tool_choice`、无法无损表达的 structured output。
- 非 vision 模型收到图片。
- Responses 的状态型能力：孤立的 `previous_response_id`、`background`、`store`/`retrieve` 等（v1 不伪造服务端持久化）。

两种模式均不支持精确 token counting（`count_tokens`）、OAuth 登录和通用 `http_request`。

## 已知限制

- **Go 套餐没有 Provider API 权限**：官方文档明确 Go 套餐调用会返回 `403 upgrade_required`。插件不会因此自动切换到 `cli` 模式，也不会把 Go 套餐描述成可调用 Provider API。
- **原生 ABI 限制**（CPA v8.0.23，零核心修改）：
  1. `pluginabi.Error` 没有 Retry-After / 错误响应头，宿主 `rpcError` 只保留 Code/message/statusCode，**上游 429 的精确等待时间无法无损传到 CPA**。
  2. RPC 解码不保留 `Retryable`，冷却/故障转移由宿主按自身策略决定。
  3. `host.stream.emit/close` 的错误是字符串，**流中（已输出内容后）的 402/429 无法让 CPA 的失败记录/冷却识别精确上游状态**。
  因此插件只在**返回成功流之前**做有界 preflight 以便尽量走失败 envelope；一旦已输出内容，只发送明确的协议错误，绝不自行换 key 重放。
- **Observer v0.4.4 不能“自动兼容”**：宿主转 `UsageRecord` 时丢弃 TokenBreakdown，且 `commandcode + executorAdapter` 对 Observer 的 SDK 是未知 provider → `unclassified`。**即使插件 payload 完全正确，当前 Observer 的非零价格仍会显示未定价**。要获得非零可定价成本，需按设计文档独立修改 Observer 增加精确 cohort（provider=`commandcode`、executor=`executorAdapter`、model namespace=`commandcode/`），**不修改 CPA 核心**；本仓库**不直接改 Observer**，该 cohort 更新仍待办。若保持 Observer v0.4.4 不变，只能承诺显示 token 与原始总量。
- **合成 workspace 接受性**未知：是否影响上游 prompt/工具行为需要受控验证。
- **无真实 usage / TTL / 缓存政策验证**：结构稳定与 session affinity 是措施，命中率与扣费不能离线保证。
- 未测试的 CPA 版本不做支持承诺。

## 条款与合规风险

CommandCode 服务条款限制逆向工程、未经授权的衍生作品、转售/为第三方提供服务，以及一人控制多账号绕限。因此：

1. CLI transport 的技术设计**不能被表述为**“官方支持的第三方 API”。
2. 面向公开发布或长期使用，应先确认上游授权/条款适用性；未确认时优先官方 Provider API 或官方 Headless。
3. 密钥池只承载**有权使用**的凭据；同账号多 key 不代表多份额度，不用于多账号促销套利或绕限。
4. 不复制分发官方 CLI 源码/系统提示词。

以上是工程风险提示，不构成法律意见。

## 与真实上游的验收状态

- 全部集成测试在**本地 loopback 合成上游**上运行；mock 的 usage、错误、响应形状都是人工构造，只证明插件/宿主行为，**不证明真实后端今天返回相同结果**。
- 示例配置与测试**不含任何真实凭据**；测试中的每个 key 都是合成的。
- 兼容性事实来自前期公开文档/模型目录研究及本地 CLI 协议审计；**本轮实现验收未使用真实凭据，未调用真实推理或 billing 端点**，未验证真实套餐权限、配额 schema、缓存效果与扣费。真正后端验收需另行获得账号与调用授权并明确预算。
- 合成 workspace 是否被上游接受、私有 endpoint 的实际接受条件，均待受控验证。

## 验证与发布

```bash
make test         # go vet ./... + go test ./... + Node UI 测试
make race         # go test -race ./...
make static       # 无需业务包/编译器的交付静态检查（静态测试 + 打包测试）
make build        # 原生 c-shared 插件
make package      # 打包单个平台 ZIP
make integration  # 真实宿主测试，必须设置 CPA_BINARY=/abs/path/cli-proxy-api
```

- `make integration` 使用 `-tags=integration`，**要求** `CPA_BINARY` 指向真实 CPA v8 宿主二进制；缺失时**硬失败**（退出码 2），绝不静默跳过。
- 集成矩阵：2 模式（`cli`/`provider`）× 3 客户端协议 × 3 模型家族 × 流式/非流式 = **36 cell**，并额外覆盖 header 顺序（raw TCP 抓包，不用 `httptest` 的 header map 当顺序证据）、usage/工具/reasoning、亲和与 429/402 故障转移、unsupported 语义零上游调用、ZDR 422 不降级、key 导入/重启/删除/quota、UTF-8/EOF/错误/取消/关停。
- CI（`.github/workflows/ci.yml`）：`go vet` / `go test` / `go test -race` / Node UI 测试 / 三平台原生构建 / 真实宿主集成（`v8.0.23`，通过可复用 `real-host.yml`）。
- 发布**仅人工**：`.github/workflows/release.yml` 由人工 `vX.Y.Z` tag 或手动 `workflow_dispatch` 触发；仓库内**没有** auto-release、自动 push、自动合并或定时改代码的工作流。发布前对准确 tag 执行 test/race/static/真实宿主集成门禁，再原生构建三平台；核验 ZIP 内容、动态库架构、metadata 版本/SDK/提交及 SHA256 后，先建草稿、上传全部资产才正式发布，并使用 `gh release create --verify-tag`（从不创建 tag）。

## 安全与许可证

- 插件资源（`/ui`、`/ui.js`、`/ui.css`）无密钥、同源 CSP；管理 API 需管理密钥；列表绝不回显 key。
- 不把 key 放进 YAML、URL、浏览器持久存储、日志或错误正文；不信任上游回显，错误文字去敏限长。
- 本仓库为 MIT 许可（见 `LICENSE`）；任何复制自参考实现的代码都按要求保留其版权与许可。
