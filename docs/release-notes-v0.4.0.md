# CommandCode Provider v0.4.0

CLI 模式的插件可控请求层对齐本地审计的 **cmd 1.88.5**；继续使用 CPA v8.0.23 的公开 SDK 与 `host.http`，不修改 CPA、不引入独立网络运行时。

## 变更

- **统一身份**：以选中凭据为秘密根，通过用途隔离的 HMAC-SHA256 派生 v4 格式稳定 UUID。CPA `canonical_session_id` 是唯一会话权威，不对消息内容重新哈希；同一凭据/auth/session 跨请求和重启稳定，换 key/auth/session 隔离。header `x-session-id` 与 body `threadId` 共用身份。
- **合成 workspace**：固定 64 项 login 与 64 项项目池，每凭据稳定选择 4～8 个唯一候选，再按会话稳定选择。`workingDir=/home/<login>/proj/<project>`；`x-project-slug` 从完整 workingDir 派生。不读取或创建宿主目录，不编造 Git 历史。
- **请求形状**：完整头顺序包含首位小写 `host` 与末位小写 `content-length`；保留双值 Content-Type。普通聊天省略 `mode`/`promptCache`；string system 保持 string，结构化 system 与显式 cache/TTL 保留，空 text/reasoning 保留 `text:""`。JSON 不转义 HTML、U+2028/U+2029，无尾换行；工具原始数值和键序不改写。
- **Brotli**：发送 `accept-encoding: br, gzip, deflate`，新增纯 Go Brotli 解码，复用解压后 64 MiB 总量与 1 MiB 单行上限。损坏数据明确失败，pause_turn 续跑复用原 body/身份并累计 usage。
- **模型快照**：CLI 1.88.5 内联目录 110 项，匿名公开目录快照 95 项，默认交集由 87 升至 95。版本、bundle SHA、快照日期一起更新。提供离线静态提取/check/diff 工具，不执行 bundle，不在运行时下载 CLI 或追踪最新版本。
- **展示**：设置页继续显示后端兼容版本；缺失时显示“未知”，不再伪造旧版本兜底。

CLI 快照来源 SHA-256：`c75b34df6487de034c4419b8919432309c966505c20b76a361dea9db80cda2fc`，日期 `2026-11-09`。CLI 相对旧快照新增 17、移除 2、变更 4；唯一能力字段移除发生于 `inclusionai/ling-3.1-flash:free`（不再声明 reasoningEfforts/maxOutputTokens），未知能力不补造。

## 明确保留的差异

- Go TLS ClientHello 与原生 Node/OpenSSL 不同；宿主禁用连接复用，插件发送单条 `connection: close`，不伪称原生 keep-alive。
- UUID 是稳定派生的 v4 格式，不是原生随机 UUID。workspace 是合成池；每次逻辑请求使用当前 UTC 日期，跨日环境前缀可能改变。
- 业务语义优先：客户端显式 system/cache/tool JSON 不为模拟字节外观而静默删改。不引入通用 JavaScript JSON.stringify 运行时。
- 不模拟 cmd 遥测、其它端点序列、OAuth、硬件身份；不新增 Node、uTLS、sidecar、插件代理管理、连接池、持久化会话表或后台轮询。
- 本地 mock 只能验证插件与宿主行为，不证明真实上游接受性、缓存命中率或扣费；不宣称“上游不可区分”。

## 升级

在插件商店升级或替换对应平台动态库并重启 CPA，保留配置与凭据。管理接口、每凭据模式、Provider 传输与配额规则不变。CLI 身份算法与合成 workspace 直接替换旧值，不提供旧 UUIDv5 兼容或迁移表；已有旧上游 thread/热缓存不会延续。

## 验证

- 单元测试覆盖身份稳定/隔离、候选池唯一性、JSON 转义边界、system/cache/空文本、Brotli 解压上限与损坏输入。
- 真实未修改 CPA v8.0.23 + loopback 合成上游：36-cell 双模式三协议矩阵；完整有序头及拒绝额外/重复/错误大小写；session 亲和和故障转移；Brotli 三协议流/非流；压缩 pause_turn 续跑身份/body 复用与 usage 累加。
- 发布门禁：`make test`、`make race`、`make static`、`make build VERSION=0.4.0`、`make integration`。发布工作流在准确 tag 上再次执行门禁，再构建 linux/amd64、linux/arm64、darwin/arm64 三个平台并校验版本、SDK、提交和 SHA256。
- 无真实凭据或收费推理调用；SDK 仍固定 v8.0.23。
