# CLI 请求指纹轻量重构方案与工作量评估

## 1. 已确认决策与最终定位

本方案按用户最新决定制定，**取代此前的插件自建 Go/uTLS 出站方案**：

- 不修改 CPA，不升级其固定 SDK 来换取新接口。
- 继续通过宿主 `host.http` 发送请求，沿用其代理、取消、超时及资源管理行为；不在本轮修复或重新定义宿主代理语义。
- 不引入 Node、uTLS、自建 HTTP 客户端、连接池、sidecar 或后台轮询。
- CLI 模式尚未使用，不兼容旧 UUIDv5、固定 workspace、旧函数签名或旧 wire fixtures，不做身份迁移表和双版本运行逻辑。
- 保留 CPA 的 canonical session 识别、选凭据、亲和、冷却及故障转移职责。

**目标：在已有宿主传输边界内，对齐插件可控的 generate 请求头、身份、workspace、body 和压缩能力。** 不能将成果表述为“完全相同”“上游不可区分”或“保证缓存命中”。

基线固定为本次已检查的 cmd **1.88.5** + Node **22.23.4** / OpenSSL **3.6.5** / undici **6.29.1** 的 API-key、production、普通聊天路径；CLI/runtime 版本变化必须重新核对，不能只自动替换版本头。

本文件是计划与估算，不代表代码已实现；生产实现仍以 `docs/contracts.md` 为准。

## 2. 明确保留的差异与不做事项

| 项目 | 本轮处理 |
| --- | --- |
| TLS ClientHello、TLS 恢复/record 行为 | 保留宿主 Go transport，不实施原生 Node 模拟 |
| Connection / 连接复用 | 宿主强制关闭连接；插件发送单个 `connection: close`，不伪称 keep-alive |
| 日期生命周期 | 每次逻辑请求取 UTC 日期，pause 续跑复用同一 body；不新增会话日期缓存或持久化。跨日可能改变环境前缀，与长驻 CLI 缓存 config 有差异 |
| 任意客户端 system/tools/cache 标记 | 优先保留业务语义；不能表达为选定原生路径的输入列为支持范围差异，不静默删改 |
| 特殊 JSON 数字、对象键序 | 不为 raw 字节相等改变业务数值，不实现通用 JavaScript JSON.stringify 运行时 |
| cmd 遥测与其它端点序列 | 不额外调用 lifecycle-events/fingerprint/更新检查，不伪造硬件信息 |
| OAuth / ACP / RPC / feature 子调用 | 不在本轮扩展；其条件头/字段不混入普通聊天 |
| Provider 模式和配额功能 | 不重构传输、调度和接口；共享版本/目录元数据变动需要必要的回归检查 |

保留宿主 HTTP 不等于完整修复代理继承：当前 native ABI 的有效代理范围沿用宿主既有实现，本轮不再新增插件代理配置，也不宣称已经获得所有 per-auth/request proxy override。

## 3. 总体结构：纯函数身份，单次组装，现有执行链不动

```text
CPA 选定 AuthID + 凭据
CPA 给出 canonical_session_id
          │
          ▼
纯函数派生 UpstreamIdentity
  ├── v4 格式稳定 ThreadID
  ├── 固定凭据级 home login
  ├── 会话稳定选中的项目/workingDir
  └── 从完整 workingDir 派生的 ProjectSlug
          │
          ├── RequestHeaders(key, identity, zdr)
          └── BuildRequest(payload, options{identity, model, now, capabilities})
                         │
                         ▼
现有 host.http operation / NDJSON / pause_turn / usage / CPA 下游流
```

- 不再由 headers.go 和 request.go 各自选一个固定项目值。
- 一次 `prepareCLI` 生成一份身份，header/body 共用，防止两侧独立计算造成不一致。
- 不让编码包读取 auth-dir、宿主 cwd、Git、机器身份或其它凭据。
- 不改变 Bridge、Manager 取消注册、hostStreamReader、响应状态机和错误重试职责。

## 4. 分模块改造

### A. 会话身份：无状态 v4 格式派生

保留 `canonicalSessionID` 及所属模型的 before-auth affinity 适配。替换 UUIDv5 输出算法：

```text
scope = 明确分段编码的 [identity-scheme, selectedAuthID, canonicalSessionID]
digest = HMAC-SHA256(选中凭据派生的密钥, scope)
ThreadID = digest 前 16 字节，设置 v4 version 和 RFC variant 位
```

要求：

- `x-session-id == body.threadId`。
- 同一凭据/AuthID/session 跨请求、跨重启稳定；不把 model 纳入 scope。
- 更换 auth、会话或凭据材料时隔离；不输出摘要密钥或凭据到日志。
- 使用独立用途标识派生 thread、用户名、候选池和池内选择值，不复用 UUID 字段作为索引。
- 仍在缺少 auth/canonical 时失败，不随机退化，不做 body 内容哈希。
- 精确称为“v4 格式稳定派生 UUID”，不声称生成过程等于原生随机 UUIDv4。

CLI 尚未使用，直接删除旧 UUIDv5 的专用实现与旧 golden，改为新算法的稳定向量和隔离断言。

### B. workspace：固定池与会话稳定选择

采用 Python adapter 的分层思路，但不复制其协议错误：

- 固定 64 项 ASCII 项目名称池和固定 login 池。
- 每凭据确定性取出不重复的 4～8 个项目候选。
- `(AuthID, canonical session)` 在该凭据候选中稳定选择。
- `workingDir=/home/<login>/proj/<project-name>`。
- `ProjectSlug=slugify(完整 workingDir)`，不是 basename。
- 路径限定为已知 ASCII 字符集；在这一受控集合上复现原生 slugify 结果，不引入通用 Unicode transliteration 依赖。
- 不同会话可以共享项目，但 thread 独立；不假定每把 key 对应真实独立账号。

workspace 保持最小合法形状：Linux、非 Git、空 structure/branch/status/commits。不从旧 adapter 移植合成 Git 历史。

候选列表与派生算法固定版本，避免普通 CLI 版本升级顺带改变项目归属。全部纯函数，不增加 session 表、TTL 清理任务或磁盘文件。

### C. HTTP headers/profile：修正可控部分，承认 Connection 差异

继续返回现有 `pluginapi.HTTPWireProfile`：

- `HTTP1Only=true`、`DisableAutoCompression=true` 不变。
- profile 增加首项小写 `host`、末项小写 `content-length`。宿主仍管理其值，插件只指定位置/大小写。
- `connection` 改为显式单值 `close`，避免宿主追加 close 后形成矛盾双头。
- `User-Agent: cli`、合并双值 Content-Type、默认 fetch 头和 traceparent 基本形状保留。
- version 绑定审阅后的 1.88.5 profile；ProjectSlug/ThreadID 从统一 identity 取值。
- 普通请求不补 surface、OSS provider、OAuth 头；ZDR 继续 opt-in。

**已经验证的落地证据：** 使用仓库真实 RequestHeaders/BuildRequest、CPA v8.0.23 的 httpwire 实现与等价 stdlib transport 设置，在临时目录复现并断言通过：

```http
POST /alpha/generate HTTP/1.1
host: <authority>
connection: close
content-type: application/json, application/json
User-Agent: cli
...其它显式头...
content-length: <真实 body 字节数>
```

已确认 Connection 只有一条、host 首位、content-length 末位；这是等价配置最小验证，最终仍需真实 CPA 加载插件验收，不冒充已完成宿主集成。

### D. Brotli：小范围新增解码，不扩大传输职责

- 在 `NewDecoder` 中增加 `br` 流式 reader。
- 继续将解压后的 reader 包在现有总大小限制内，保留单行与 malformed/terminal 校验。
- 成功支持后才把声明改成 HTTPS 基线的 `br, gzip, deflate`。
- 用纯 Go Brotli 库。当前 go.sum 已有 `github.com/andybalholm/brotli v1.0.6` 校验记录，但 go.mod 未直接声明，实施时固定并审阅直接依赖。
- 不为此引入 uTLS、Node、其它压缩算法或后台服务。

### E. body/JSON：表示修复，业务语义优先

1. **输入身份统一**：RequestOptions 携带同一 UpstreamIdentity；config 从该身份填充，不再使用独立固定 workingDir。
2. **空字符串**：text/reasoning 块显式保留 `text:""`；不让通用 `omitempty` 把空文本变成缺失字段；其它类型不混入多余 text。
3. **system union**：支持 string 与 text-block 数组。字符串保持字符串，结构化输入保留分段与显式缓存语义；必要时检查 SDK 转译是否丢失来源表示，用请求局部信息恢复，不借此修改客户端正文。
4. **序列化**：关闭 HTML escaping、去掉 Encoder 附加的末尾换行；U+2028/U+2029 的处理必须识别 JSON 转义边界，不能用盲目全局替换破坏用户字面量。
5. **工具/历史**：保留 tools、tool-call input、tool-result、image、reasoning 的既有转换语义；检查键序与空/null 处理。不会为模仿 JS 把大整数转成 float64 或篡改数值。
6. **可选字段**：继续省略普通主聊天的 mode/promptCache；不补 null，不加入未验证 feature 字段。
7. **生成控制**：客户端合法显式 temperature/max_tokens/effort 继续保留。模型默认值与 cap 在核对新目录及调用层后调整，不仅凭“CLI 变默认模型”推断 effort 或 token 上限变化。
8. **缓存标记**：显式 TTL/message cache_control 不静默删除；与原生 mapper 不同的部分记录为语义保留差异，而不是伪造“逐字节完全匹配”。

### F. 版本与目录快照：补齐发布一致性，不运行时追最新

当前 `catalogdata/data.go` 同时维护 CLIVersion、bundle hash、SnapshotDate；`cli.json` 也记录相同来源信息。更新时：

- 静态核查 1.88.5 的模型 ID、vision、efforts、context/max-output 等确有证据的事实，生成可 review 的候选 diff。
- 原子更新版本、来源 hash、CLI 能力快照及对应 fixtures。
- 保留现有 CLI/public 交集与 manual-models 规则，不顺带扩大默认路由或推断套餐权限。
- 若需刷新 public 快照，只使用公开匿名接口，保留来源日期；不凭空重写 Provider endpoint 事实。
- 仓库目前没有目录提取器；可增加一个接受本地 bundle 路径的维护期静态提取/差异脚本，不执行 bundle、不加入运行时 npm 下载或 CI 自动发布。
- Python adapter 的提取器只扫命中后的固定 2500 字符，存在跨对象取错字段风险，不原样搬运；候选必须按实际对象边界识别并 review。
- UI 不新增页面/设置项，仅移除旧 1.79.2 的硬编码展示兜底，继续显示后端兼容版本。

历史 release notes 与调查中引用的旧版本不批量替换。只更新当前契约、README 的当前实现说明和本次发布说明。

## 5. 变更范围及不触碰范围

### 核心文件

| 范围 | 预计落点 |
| --- | --- |
| 统一身份与 workspace | 新增 `internal/commandcode/identity.go`、`workspace_pool.go`；修改 provider/session.go |
| 编码输入与组装 | commandcode/types.go、request.go；provider/cli.go 的 prepareCLI |
| 头与 wire profile | commandcode/headers.go |
| 压缩 | commandcode/decoder.go、go.mod/go.sum |
| 小范围 JSON 辅助 | 需要时新增 commandcode/jsonwire.go，不引入通用 JS 解释器 |
| 快照/维护工具 | catalogdata/data.go、cli.json、必要的 public snapshot、一个本地候选提取脚本 |
| 展示 | web/ui.js 的版本兜底及相应定向断言 |
| 验收 | 对应单元测试、integration/wire_test.go、affinity_test.go、必要的目录/control 断言与 fixtures |
| 文档 | 当前 contracts、README、发布说明；本方案记录已知例外 |

### 明确不重构

- main.go 的 C ABI、Bridge HTTP 方法、Manager operation/cancel、上下游 stream pump。
- CPA 核心、SDK types、代理和连接池管理。
- key 导入/管理、路由/冷却调度、Provider transport、配额规则及 UI 架构。
- 新 lifecycle capability、私有 RequestID header、CancelFunc 表。
- 会话迁移、磁盘映射、原生随机 UUID 持久化、长期会话环境缓存。

共享目录元数据更新可能改变模型能力展示，因此需目录/控制参数定向验证；“不重构 Provider”不代表可省略所有共享代码影响检查。

## 6. 实施顺序与最小充分验证

### 1）定快照与内部输入契约

- 核对新 CLI 能力候选，固定来源和例外清单。
- 确定 UpstreamIdentity、RequestOptions 与 headers 入参；不保留旧双接口。

### 2）身份与 workspace

- 定向验证固定输入稳定向量、auth/session 隔离、跨模型不变、重新实例化稳定。
- 验证池内无重复、每凭据候选数量、同一 session 的 path/slug 同源；不用反复测试无关管理字段。

### 3）headers / Brotli / body

- 完整头集合、大小写、顺序、唯一 Connection、正确 Content-Length。
- Brotli 正常/损坏/解压上限的聚焦验证；gzip/deflate 既有测试不退化。
- 字符串/数组 system、空 text、特殊字符/字面转义、工具历史和显式缓存标记的少量代表 fixture。
- 所有语义拒绝与错误脱敏规则继续有效。

### 4）未修改 CPA 的集成验收

复用现有 raw TCP synthetic upstream，不重建 E2E 框架：

- 将现有“大小写不敏感的顺序子序列”改为完整有序头比较，拒绝额外/重复头；明确原生 keep-alive 与本方案 close 的已知例外，不把例外算成一致。
- 同一 CPA session 使用稳定身份/凭据；更换 session/auth 后隔离，header/body UUID 相等且版本位为 4。
- 检查代表性的 Chat/Messages/Responses 转译，以及 pause_turn 的 body/身份复用；不同协议都经过同一改造 builder。
- 上游 gzip/Brotli 响应、首帧前错误和现有取消/停用机制不退化。
- 必要的 Provider/管理 settings 冒烟，仅覆盖共享版本/目录或混合模式影响，不扩为 UI 全覆盖。
- 原生对照 fixture 只归一化真实动态字段；不得把 UUID version、字段有无、额外头、类型差异顺手忽略。

最后跑相关单测/race、native 构建与现有静态检查。真实上游接受性、扣费和缓存命中不由 mock 保证，需要时单独受控验收，不计入本轮离线重构完成条件。

## 7. 改造量级

**结论：中小型、局部请求封装重构，不是传输架构重写。** 不是几行版本/头替换；复杂度主要在身份统一、JSON 表示、模型来源核对和严格 wire 测试。

以下是基于当前代码结构的粗估，不是实际 diff 或工期承诺：

| 工作包 | 熟悉 Go 维护者的有效工时 |
| --- | --- |
| 基线、版本与能力快照核对，最小维护工具 | 4～8 小时 |
| 统一身份、稳定 UUID 与 workspace 池 | 4～6 小时 |
| 完整 headers/profile、唯一 close、Brotli | 3～5 小时 |
| body/system/空值/字符串编码修复 | 5～9 小时 |
| 定向单测、raw wire 和真实 CPA 集成 | 5～8 小时 |
| 当前文档、展示、review 与构建收尾 | 2～4 小时 |
| **合计** | **约 24～40 小时，即 3～5 个有效人日** |

变更规模粗估：

- 核心生产代码约 **8～12 个文件**，测试与 fixtures 约 **8～12 个文件**，快照/依赖/文档约 **5～8 个文件**；合计约 **22～32 个文件**，多数是同步断言/数据而非大段业务重写。
- 非生成生产代码新增/修改约 **350～650 行**，测试/测试辅助约 **450～850 行**；静态池、模型 JSON 和原生 fixture 单独统计，不用生成数据行数夸大改造。
- 运行时新增最多一个直接 Brotli 依赖；身份只用 Go 标准库。没有新增服务、定时任务、持久化表、用户配置面或网络调度器。

工时较大的不确定项：

1. **目录提取**：当前无可靠提取脚本，原生 bundle 压缩且包含多套目录；不能把宽泛匹配的命中直接当权威模型事实。
2. **system 与 JSON 的边界**：SDK 转译可能丢失原输入表示，需在有限范围恢复，不能借机写一套新协议转换器。
3. **宿主 wire 行为**：最小等价配置已验证，真实 CPA 加载新插件后仍必须验证完整头；发现无法修复的宿主差异就记录，不扩大为改宿主或自建传输。

若目录形态或 JSON 边界超出上述预期，应先报告具体新增范围，再调整估算；不把无关 UI、整套 CI 自动升级、全部历史文档或传输重写塞进本轮。

## 8. 完成标准与当前状态

完成需同时满足：

1. CLI 可控请求字段通过固定版本 fixtures；已知例外单列。
2. CPA session/凭据亲和保持，UUID 与 workspace 稳定、自洽。
3. 支持声明与 Brotli 解码一致，业务语义和错误边界未被削弱。
4. 未修改 CPA、未新增出站客户端/池/代理配置/后台服务。
5. 当前契约明确保留 TLS、close/不复用、日期生命周期及无法无损归一的业务字段差异。

当前仅完成方案与量级评估。补充的本地最小 wire 验证已证明单 close 与完整首尾 profile 可行；尚未实施本方案、未运行改造后的插件集成测试、未进行真实上游推理验收。
