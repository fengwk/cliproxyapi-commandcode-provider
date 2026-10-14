# CommandCode Provider

`cliproxyapi-commandcode-provider` 是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）v8 的**原生插件**（C ABI `c-shared` 动态库），把 CommandCode 作为 CPA 的上游 provider 接入。展示名 **CommandCode Provider**，稳定技术 ID `cliproxyapi-commandcode-provider`，provider / 公开模型前缀统一为 `commandcode` / `commandcode/`。

- 客户端协议：OpenAI Chat Completions、Anthropic Messages、OpenAI Responses，均支持流式与非流式。
- 每个凭据显式选择 `cli` 或 `provider`；YAML `mode` 只是缺省默认（`cli`），**不检测套餐、不因 403 或其他错误自动降级或切换**。
- 只读配额查询（CPA 原生 `QuotaProvider`），不轮询、不写回上游。
- 本仓库是独立插件仓库，不 fork、不修改 CPA 核心，只使用 CPA v8.0.23 的公开 SDK。

> 本文档描述**已实现的插件契约**。除特别说明外，所有上游行为均以合成的本地 mock 验证，**没有对真实 CommandCode 服务做过生产验收**（见“与真实上游的验收状态”）。安装可使用插件商店或手动构建方式。

> 每凭据模式从 **v0.2.0** 起提供；v0.1.1 不包含该功能。请在插件商店刷新并升级到 **v0.2.2**，保留既有配置与凭据。v0.2.2 修复协议终态、流中断及管理列表错误处理，详见 [发布说明](docs/release-notes-v0.2.2.md)。

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
make package VERSION=0.2.2   # 生成插件商店 ZIP（含 metadata.json 与 .zip.sha256）
```

`make package` 的归档名与 `metadata.json` 使用 `VERSION`：显式传 `VERSION=0.2.2` 时归档为 `cliproxyapi-commandcode-provider_0.2.2_<goos>_<goarch>.zip`；不传则非 tag 本地构建使用 `0.2.0-dev+<short-sha>`。发布由 tag 决定版本。

把动态库放到 `<plugins.dir>/<goos>/<goarch>/cliproxyapi-commandcode-provider.{so,dylib,dll}`。注意 Linux 产物依赖 glibc，**不适用于 musl/Alpine**。

## 配置 CPA

参考 `config.example.yaml`（合并进既有配置，不要直接覆盖）。最小配置面：

```yaml
plugins:
  configs:
    cliproxyapi-commandcode-provider:
      enabled: true
      mode: cli                 # 仅缺省默认：cli | provider；不覆盖已有 per-auth mode
      base-url: "https://api.commandcode.ai"   # 仅 authority，不含 /alpha 或 /provider/v1
      zdr: false                # Zero Data Retention 默认关闭（opt-in）
      # models: [...]           # 可选白名单，不创造目录外路由
      # manual-models: [...]    # 显式新增/覆盖模型 metadata
```

- **`mode`**：只作为**旧凭据默认模式**。没有根级 `mode` 的旧 auth，以及导入请求未传 `mode` 时，使用该值（缺省 `cli`）。已写入 auth 的显式 `mode` 不受它改变；改这个 YAML **不会**把所有凭据切到同一模式。`cli` 走 `/alpha/generate`；`provider` 走 `/provider/v1/*`。非法 YAML 保留旧配置快照，不产生半切换。
- **`base-url`** 只写 API authority，可选带一个**路径前缀**（例如反向代理下的 `/commandcode`）；**不要**附加 `/v1`、`/alpha` 或 `/provider/v1`，插件在 authority+前缀之后自行拼接版本化路径。生产必须使用 HTTPS；禁止 URL 中含 userinfo/query/fragment。
- **`zdr`** 默认 `false`（与原 CLI 一致）；`true` 时发送 `x-cmd-zdr: 1`，遇到 `422 cmd_zdr_no_providers` 原样上报，**不会撤掉 ZDR 再重试**。
- **`models`** 只是白名单，不新增目录外模型。
- **`manual-models`** 是两种模式共享的模型元数据：`{id, protocol?, context-window?, max-output-tokens?, input-modalities?, reasoning-efforts?}`，`protocol` 为 `openai` / `claude` / `openai-response`。结构校验与默认模式无关。能否发布或执行取决于**当前凭据模式**：Provider 需要手动 `protocol` 或当前目录里可证明的 chat endpoint；CLI 拒绝显式声明了 text/image 以外输入的条目。上限 100 条、每条 id ≤ 200 字节。
- **宿主保留字段**：`enabled`、`priority` 和商店写入的 `store` 由 CPA 管理；插件接受但不把它们作为业务配置。`store` 内的版本、安装来源及其他元数据不影响传输模式或上游地址；其他未知顶层字段仍严格拒绝。

模式是**每凭据显式**的：插件不检测套餐，不会因为 `403 upgrade_required` 或任何错误在 `cli` 与 `provider` 之间降级或切换，也不会自动回退到某个固定模型列表。

### 凭据 JSON

宿主保存在 `auth-dir` 的物理记录带根级 `mode`。示例（占位 key，不是真实凭据）：

```json
{"type":"commandcode","api_key":"replace-with-commandcode-key","mode":"cli"}
```

`mode` 只能是 `cli` 或 `provider`。旧记录可以不写该字段，执行时采用 YAML 默认值；一旦写上显式值，改 YAML 默认值不会改变它。空值、类型错误或非法值失败关闭，不改用其他来源。不要把 key 放进本仓库或 YAML。

### 商店安装后显示“未注册”

v0.1.0 遗漏了宿主保留字段 `store`，商店安装后可能在 `plugin.register` / `plugin.reconfigure` 返回 `invalid config yaml`；全局和实例开关开启也无法解决。这同时会导致插件名称、可视化配置字段和管理菜单缺失。

**v0.1.1 起已修复**：刷新插件商店，升级或重新安装当前版本 **v0.2.2**。保留现有插件设置及凭据，无须移除 `store` 元数据或修改 CPA 核心。若之前为临时恢复而删除了 `store`，通过商店安装当前版本，让 CPA 重建对应新版本的元数据；不要原样恢复旧 `store.version`，以免重新选择旧库。反复安装同一个 v0.1.0 不能解决问题。

## 导入 CommandCode 密钥

密钥经插件管理路由导入，由宿主保存到 CPA `auth-dir`（默认 `0600`），**不写入 YAML、日志、URL 或浏览器存储**。

- 管理界面（同源嵌入）：`GET /v0/resource/plugins/cliproxyapi-commandcode-provider/ui`
- 列表：`GET .../keys` → `{files:[{name,auth_index,label,status,disabled,unavailable,success,failed,mode,mode_source,mode_error}]}`。`mode_source` 为 `auth`（记录内显式）或 `default`（采用 YAML 默认）。无法确定时 `mode=""`、`mode_source=""`、`mode_error=true`，不伪装成 `cli`；单条失败不阻断其他条目，响应不含 raw JSON、key 或路径。
- 导入：`POST .../keys`，体如 `{"keys":["replace-with-commandcode-key"],"label":"...","mode":"provider"}`。`mode` 可选 `cli` 或 `provider`，省略时用 YAML 默认；UI 导入下拉默认选 CLI，并显式提交所选值，只作用于本次导入。非法、空、重复、未知字段或尾随 JSON 在零写入前拒绝。新凭据持久化该 `mode`。重复 key 仍 `skipped`，不改既有凭据模式。返回 `{imported,skipped,failed}`。
- 修改模式：`PATCH .../keys`，体如 `{"auth_index":"...","mode":"provider"}`（≤64KiB，无重复字段或尾随 JSON）。只接受 `auth_index` 与 `mode` 两个字段，后者必须显式为 `cli` 或 `provider`。只改该凭据的根级 `mode`，保留 `api_key`、`disabled`、`label`、`prefix`、`proxy_url`、`weight` 以及未知字段和精确 JSON 数字。只接受本 provider 的安全 flat `.json`；未知或外来 auth 为 404，读取或保存失败为 502，错误文案固定且不回显凭据。成功 `200`：`{auth_index, mode, mode_source:"auth"}`。
- UI 每行有模式下拉和“保存模式”。保存后重新 GET，确认该条 `mode` 为目标且 `mode_source=auth`；未确认不等于已生效。非法或不可读显示错误，不伪装成 CLI。错误不清空列表或其他凭据。设置区的“旧凭据默认模式”只展示 YAML 默认，不是所有凭据的模式。
- 删除：使用 **CPA 原生**认证文件管理路由 `DELETE /v8/management/credentials?name=<file>`；插件**没有** `host.auth.delete`。
- Settings（只读）：`GET .../settings`。`mode` 与 `default_mode` 都是旧凭据默认值；另有 `mode_scope:"auth"`、`catalog`（该默认值下的目录状态）、`catalogs.cli` / `catalogs.provider`，以及 `base_url`、`zdr`、`cli_version`、`manual-models`。

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

CPA 仍按模型选凭据；插件不自建选 key 调度。`model.for_auth` 与执行使用同一套严格解析：最新 AuthMetadata > AuthAttributes > StorageJSON > YAML 默认。字段缺失才看下一来源；显式存在但类型错误、空值或非法则失败，不降级。执行前把所选模式绑定到**当前请求**的配置副本；异步执行和续跑沿用该快照，不改共享配置。

- **CLI 凭据**：发布嵌入的 CLI/public 交集（默认 87 个，白名单与合格手动模型会改变数量）；上游 wire 不因客户端协议改变，均经 `/alpha/generate` NDJSON。CLI-only/别名条目需要显式 `manual-models` 注册。显式声明 text/image 以外输入的手动条目不发布、不执行。
- **Provider 凭据**：即使 YAML 默认是 `cli`，也会触发匿名 `GET <base-url>/provider/v1/models`（不携带凭据），并以当前目录的 `supported_endpoints` 路由：
  1. 模型声明了客户端原协议 → 直接用同协议 endpoint。
  2. 否则选择目录声明支持、且公共 SDK 已注册双向转换的确定性 endpoint。
  3. 无可用映射 → 本地 `400`，绝不把请求发到错误 endpoint 再“试另一种模式”。
  手动条目若没有 `protocol`，也没有当前目录可证明的 chat endpoint，则不发布、不执行。
- 插件默认模式的 `model.static` 只作兼容展示，不参与凭据模式判定，也不扩大该凭据的模型集。
- 目录发现失败保留 last-good 并标记 stale，**不回退为空目录或 CLI**；发现失败不伪装成成功。

## 只读配额

配额走 CPA 原生插件 quota 路由，按 `auth_index` 单次触发，不轮询、不重试、不缓存、不支持“重置上游额度”：

```
POST /v0/management/plugins/cliproxyapi-commandcode-provider/quota
Body: {"auth_index":"<来自 keys 列表的 index>"}
```

`groups` 按 `five-hour`、`weekly`、`monthly` 顺序给出可信窗口：五小时/周窗口只取已知 `cap>0` 的真实数据，`remainingFraction = clamp(1 - used/cap, 0, 1)`，`resetTime` 来自毫秒时间字段；缺失窗口不伪造为 0。

`monthly`（月度额度）只有在能从可信来源得到月度总额时才发布，`remainingFraction = clamp(monthlyCredits / total, 0, 1)`，其 `description`（纯文本，经控制字符/长度清洗）注明总额与来源：

- `/alpha/billing/credits` 返回可用的本期授额 `monthlyCreditsGranted`（可选字段）时，`total = max(grant, 官网套餐基准)`；该字段非法（类型错误/溢出/负数）只禁止月度且**不**回退静态基准，不破坏可信的三项余额、`total` 与五/周窗口；`null`/缺失视为缺失，`0` 走基准；
- 否则按**带日期的官网套餐基准快照**（2026-10-14，来源 commandcode.ai 公开静态 `plan-tiers` 资源）：已知套餐用其基准，`teams-pro` 按有效席位（`floor(quantity)`，缺失或 `<1` 记 1）乘每席位基准；席位 `quantity` 非法只禁止 teams-pro 基准分支（不默认成 1 席），不影响个人基准或有效授额；未知套餐且无授额时**不发布**月度窗口；
- `past_due`、月余额缺失、非法授额、总额 `≤0` 或溢出均不发布月度窗口；`past_due` 不因可选 `quantity` 类型异常而失效；`resetTime` 只取真实账期（RFC3339 或 `YYYY-MM-DD`），**不推测固定自然月**；明确非空 `cancelAt`（取消）或 `pendingPhase`（待生效）时只省略 `resetTime`（`null`/缺失/空 `cancelAt` 仍走真实日期），`remainingFraction`/来源/`summary`/五小时与周窗口不变。

`summary` 只展示 credits API 明确给出的 `monthly_credits`/`purchased_credits`/`free_credits` 与同账号 `total_credits`，`unit=credits`、`format=number`，**不猜 USD**。月余额（`monthly_credits`）是剩余量，**不是**月度上限；不把它当分母，也不把旧额度、充值或免费余额混入月 cap。

官网 Web 走 `/internal/billing/**`，插件与 CLI 走 `/alpha/billing/**`；公开静态资源证明的字段形状**不等于**每个账号的 `/alpha` 响应都会返回 `monthlyCreditsGranted`。本仓库在**无真实账号/凭据**下验收，**未实测真实账号返回**。

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

- **Go 套餐没有 Provider API 权限**：官方文档明确 Go 套餐调用会返回 `403 upgrade_required`。插件不检测套餐，也不会因此把该凭据降到 `cli`，或把 Go 套餐描述成可调用 Provider API。需要 CLI 传输时，为该凭据显式选择 `cli`。
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
- 每凭据混合模式集成覆盖：不同 auth 走不同 endpoint、Provider-only 模型只选 Provider auth、CLI-only 模型只选 CLI auth、热修改及重启持久化、旧记录采用默认值且显式 mode 不受默认值改变、非法模式不发上游、外来 auth 禁止修改。
- CI（`.github/workflows/ci.yml`）：`go vet` / `go test` / `go test -race` / Node UI 测试 / 三平台原生构建 / 真实宿主集成（`v8.0.23`，通过可复用 `real-host.yml`）。
- 发布**仅人工**：`.github/workflows/release.yml` 由人工 `vX.Y.Z` tag 或手动 `workflow_dispatch` 触发；仓库内**没有** auto-release、自动 push、自动合并或定时改代码的工作流。发布前对准确 tag 执行 test/race/static/真实宿主集成门禁，再原生构建三平台；核验 ZIP 内容、动态库架构、metadata 版本/SDK/提交及 SHA256 后，先建草稿、上传全部资产才正式发布，并使用 `gh release create --verify-tag`（从不创建 tag）。

## 安全与许可证

- 插件资源（`/ui`、`/ui.js`、`/ui.css`）无密钥、同源 CSP；管理 API 需管理密钥；列表绝不回显 key。
- 不把 key 放进 YAML、URL、浏览器持久存储、日志或错误正文；不信任上游回显，错误文字去敏限长。
- 本仓库为 MIT 许可（见 `LICENSE`）；任何复制自参考实现的代码都按要求保留其版权与许可。
