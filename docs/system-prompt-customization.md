# Command Code 系统提示词自定义能力调查（交接文档）

- 日期：2026-11-09
- 性质：交接复核文档（面向零上下文 agent，所有结论附可复现锚点）
- 关联文档：`docs/fingerprint-gap-analysis.md`（请求指纹差异分析，其中 D9 为系统提示词内容差异，本文是其能力背景）

## 0. 结论速览

| 能力 | CLI 1.79.2（provider 指纹目标） | CLI 1.88.5（npm latest，本机安装版） |
|---|---|---|
| 主会话整段替换系统提示词 | ❌ 不支持 | ✅ 支持，mod 的 `transformSystemPrompt` hook |
| 主会话追加（append） | ✅ mod `appendSystemPrompt` | ✅ mod `appendSystemPrompt` |
| 子代理完全自定义 | ✅ `.commandcode/agents/*.md` body 即完整系统提示词 | ✅ 同左，另可 `cmd.addAgent({systemPrompt})` |
| CLI flag / settings 配置项 | ❌ 无 | ❌ 无（任何版本都没有 `--system-prompt` 类入口） |
| 内部嵌入路径 `config.systemPrompt`（string 或 builder 函数） | 存在（SDK/embedder 用，用户配置不可达） | 存在 |

一句话：**主会话想整段替换，唯一途径是写一个 mod 文件；纯配置做不到。子代理则一直是 md body 直接控制。**

## 1. 被检对象与证据来源

| 对象 | 位置/来源 | 说明 |
|---|---|---|
| CLI 1.88.5 安装产物 | `/home/fengwk/.local/lib/node_modules/command-code/dist/cli.mjs` | 本机安装版；npm `latest` = 1.88.5（`npm view command-code dist-tags` 实测）；bundle 15 行压缩，验证用 `grep -o` 锚字符串 |
| CLI 1.79.2 tarball | npm `command-code@1.79.2`，解包至临时目录 `package/dist/cli.mjs` | provider `internal/catalogdata/data.go:7` 锁定的指纹目标版本 |
| 官方文档 | https://commandcode.ai/docs/mods | 2026-11-09 实时抓取，明确记载 `transformSystemPrompt` |
| 本地 bundled 知识库 | `~/.local/lib/node_modules/command-code/dist/bundled/command-code-knowledge/reference/product-help.md` | 只提到 `appendSystemPrompt`，**未同步 `transformSystemPrompt`——bundled 参考可能滞后于线上文档，复核时以线上文档 + bundle 双重验证为准** |

## 2. 机制细节（1.88.5，bundle 级证据）

主会话每轮的系统提示词管线（锚：`grep -o 'resolveSystemPrompt({config.\{0,60\}' cli.mjs` 附近）：

```
x = resolveSystemPrompt({config,...})      // 基础提示词；config.systemPrompt 为 string 直接用，为 function 则调用
I = runAppendSystemPrompt({mods,...})      // 各 mod 返回非空字符串则 '\n\n' 连接，追加在基础提示词之后
R = composeSystemPrompt({base:x, tail:[I, deferredTools]})
O = runTransformSystemPrompt({mods, system:R.system, ...})   // ← 替换点
```

`runTransformSystemPrompt` 语义（锚：`grep -o 'async function runTransformSystemPrompt.\{0,400\}' cli.mjs`）：

```js
let n = e.system;
for (const o of e.mods)
  if (o.transformSystemPrompt) {
    const r = await o.transformSystemPrompt({system:n, state:e.state}, e.ctx);
    "string" == typeof r && (n = r);   // 返回字符串 → 整段替换；链式传递给下一个 mod
  }
return n;
```

替换生效后 `systemSections` 被丢弃（`D = O===R.system ? R.systemSections : void 0`），wire 上 `params.system` 变成单个纯字符串。对照 1.88.5 generate body 构造（锚：`grep -o 'body:{config:u,memory:null.\{0,500\}'`）中的 `system:n.systemSections?toWireSystem(n.systemSections):n.system`。

`transformSystemPrompt` 已注册进合法 hook 名单（锚：`grep -o '"transformInput","transformContext","appendSystemPrompt","transformSystemPrompt.[^]]*' cli.mjs`）。

## 3. 官方文档记载（commandcode.ai/docs/mods，2026-11-09 抓取）

> `transformSystemPrompt({system, state}, ctx?) → string`
> Fires once per round, after `appendSystemPrompt`: receives the round's **full** system prompt (base + every append) and returns the prompt to send, so a mod can **replace, edit, or drop parts of it**. Threaded mod to mod. Returning the input unchanged keeps the structured, cache-friendly sections; **any change sends one flat string**, so keep the result **byte-stable across rounds** or the prompt-prefix cache misses every turn.

官方文档同时确认的三点限制：

1. **改动即摊平**：返回值与输入不同时，结构化 sections 消失，只发一个 flat string——原本挂在系统提示词里的 skills 目录等分段（`reference/tools.md:396`："The skill catalog rides in the system prompt"）会一并丢掉
2. **byte-stability**：逐轮返回不同文本会打爆 provider 的 prompt-prefix 缓存
3. **返回非字符串**（undefined/Promise）= 不修改，原样通过

## 4. 版本边界证据（1.79.2 vs 1.88.5）

```bash
# 1.79.2（npm pack command-code@1.79.2 解包后）
grep -c 'transformSystemPrompt' package/dist/cli.mjs   # → 0  ❌ 不存在
grep -c 'appendSystemPrompt'    package/dist/cli.mjs   # → 1  ✅ 仅追加
grep -c 'beforeModelRequest'    package/dist/cli.mjs   # → 0  （hook 集合更小的旁证）

# 1.88.5（本机安装版）
grep -c 'transformSystemPrompt' $CLI                   # → 6+ ✅ 存在（定义+调用+名单）
```

即 `transformSystemPrompt` 是 1.79.2 → 1.88.5 之间引入的能力（未做二分定位；如需精确引入版本，下载中间版本对 `grep -c transformSystemPrompt` 二分即可）。

## 5. 配置方法（1.88.5 主会话整段替换）

**第 1 步** — 写 mod 文件，三种作用域任选：

| 作用域 | 位置 | 生效条件 |
|---|---|---|
| 仅本会话 | 任意路径，`cmd --mod ./file.ts` 加载 | 立即；`--mod` 可重复 |
| 用户级 | `~/.commandcode/mods/*.ts` | 所有项目自动发现 |
| 项目级 | `<项目>/.commandcode/mods/` | 需先信任工作区 |

```ts
// ~/.commandcode/mods/my-prompt.ts —— jiti 直接编译 TS，无需构建
import type {ModApi} from '@commandcode/harness';

export default function (cmd: ModApi) {
  cmd.hooks({
    transformSystemPrompt({system}) {
      return "完全自定义的系统提示词";     // 整段替换
      // return system + "\n\n附加规则";  // 或编辑式改写
    },
  });
}
```

**第 2 步** — 验证与管理：

```bash
cmd mods list                  # 确认加载且无警告
cmd mods inspect ./my-prompt.ts # 静态检查注册的 hook
```

TUI 下保存文件即热重载（feed 显示 `[mods] ✓ reloaded`）；`/mods reload [name]` 手动重载；`cmd --safe-mode` 或 settings `{"mods":{"disableAll":true}}` 全部禁用。

**子代理路径**（无需 hook，任何版本可用）：`.commandcode/agents/*.md` 的 body 就是该 agent 的完整系统提示词（front matter 配 name/description/tools/model），或 mod 内 `cmd.addAgent({name, description, systemPrompt, tools, model})`。保留名 `explore`/`plan`/`review`/`general` 不可占用。

## 6. 与本 provider 的关联

1. **D9（`params.system` 内容差异）的能力背景**：真实 CLI 的系统提示词在客户端本地拼装（身份提示 + `renderEnvContext` 环境上下文 + skills 目录），provider 只转发客户端传入的 `system`。若上游客户端（经 CPA 接入方）本身是另一个 coding agent，其系统提示词内容与 cmd 原生流量天然不同——这是内容级、代理模式固有的差异，无法在 provider 侧修复。
2. **版本绑定权衡**：客户端若需要主会话整段替换系统提示词（1.88.5+ 的 mod 能力），就必须允许 CLI 升级离开 1.79.2；而升级会同时触发指纹报告 D1–D6 的全部差异。两个诉求（能力 vs 指纹锁定）互斥，需产品层决策。
3. **一个潜在用法**：客户端侧可以用 `transformSystemPrompt` 把系统提示词改写为更接近 cmd 原生样式的内容来收窄 D9 差距，但这属于客户端侧行为，provider 不感知、不控制。

## 7. 复核操作清单（零上下文可直接执行）

```bash
# 1. 确认本机版本 = npm latest
npm view command-code dist-tags --json   # latest 应为 1.88.5
cat /home/fengwk/.local/lib/node_modules/command-code/package.json | grep '"version"'

# 2. 1.88.5 存在替换 hook
CLI=/home/fengwk/.local/lib/node_modules/command-code/dist/cli.mjs
grep -c 'transformSystemPrompt' $CLI                        # > 0
grep -o 'async function runTransformSystemPrompt.\{0,400\}' $CLI
grep -o '"transformInput","transformContext","appendSystemPrompt","transformSystemPrompt.\{0,200\}' $CLI

# 3. 1.79.2 不存在（下载 tarball 验证）
cd $(mktemp -d) && npm pack command-code@1.79.2 --silent && tar xzf command-code-1.79.2.tgz
grep -c 'transformSystemPrompt' package/dist/cli.mjs        # → 0

# 4. 官方文档核对
# 抓取 https://commandcode.ai/docs/mods 搜索 "transformSystemPrompt"

# 5. 端到端验证（可选，需已登录凭据）
cat > /tmp/sp-mod.ts <<'EOF'
export default function (cmd: any) {
  cmd.hooks({ transformSystemPrompt({system}) { return "REPLACED-PROMPT-MARKER"; } });
}
EOF
cmd -p "reply with the first word of your system prompt" --mod /tmp/sp-mod.ts
# 预期：输出与替换词相关而非 cmd 原生身份词
```

## 8. 未决问题

1. `transformSystemPrompt` 的精确引入版本（1.79.2 与 1.88.5 之间，未二分）
2. 主会话运行时 `resolveSystemPrompt` 的 `config.systemPrompt` 在普通交互会话中是否恒为 undefined（bundle 中仅见 agent 定义与嵌入路径赋值；若未来版本把它接到 settings，"纯配置替换"可能出现，需重验）
3. bundled 本地参考（`product-help.md`）与线上文档的同步机制——本文写作时 bundled 只提 appendSystemPrompt，复核者应以线上 docs + bundle 双源验证
