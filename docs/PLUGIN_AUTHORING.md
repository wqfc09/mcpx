# MCPX Plugin Authoring

这份文档面向第一次编写 MCPX Plugin 的作者。Package V2 是唯一作者格式；不要编写或生成单文件 manifest，也不要直接维护 normalized `.mcp.json` Plugin shape。

稳定接口见 [`PLUGIN_DEVELOPER_CONTRACT.md`](./PLUGIN_DEVELOPER_CONTRACT.md)，架构职责见 [`PLUGIN_ARCHITECTURE.md`](./PLUGIN_ARCHITECTURE.md)。

## 1. 推荐目录

复杂 Plugin：

```text
plugins/Demo/
├── plugin.yaml
├── runtime.yaml
├── tools.yaml            # 仅 MCP runtime
├── guidance/             # 可选，给模型渐进披露
│   ├── lifecycle.md
│   └── waiting.md
├── docs/                 # 给人，不自动进入模型上下文
├── src/
└── test/
```

简单 Plugin 不需要为了形式创建空目录；没有 Guidance 就不要写 `guidance:`。

## 2. plugin.yaml：身份与组件索引

最小 MCP Plugin：

```yaml
manifest_version: 2
name: Demo
description: Demo MCPX Plugin
runtime: ./runtime.yaml
tools: ./tools.yaml
```

带 Guidance/TUI：

```yaml
manifest_version: 2
name: Demo
runtime: ./runtime.yaml
tools: ./tools.yaml
guidance: ./guidance
ui:
  tui:
    title: Demo
    command: node
    args: [./src/cli.js, tui]
```

`plugin.yaml` 不放完整 Tool schema、Inbox 业务逻辑、taxonomy 状态机或 normalized trust/enabled 字段。

可选 integration 字段：

- `requires.plugins`
- `uses`
- `watches`
- `skill_injection`
- `ui.tui`
- `guidance`

MCPX 使用 strict YAML decoding；未知字段直接失败。

## 3. runtime.yaml：怎么运行

MCP runtime：

```yaml
type: mcp
scope: workspace
command: node
args: [./src/cli.js, plugin]
env: {}
```

Native runtime：

```yaml
type: native
scope: workspace
command: node
args: [./src/cli.js, controller]
```

`mcp` / `native` 只描述运行方式。不要为了业务角色再造 `controller`、`watcher` 等 runtime 类型。

相对 command/args path 以 package 根目录解析；普通命令名（如 `node`）保持 PATH lookup。

`type: mcp` 还意味着一个重要运行时约定：**MCPX 会把 Plugin 连接当作多路复用长连接使用。** 同一个 Plugin process 里，业务 Tool、`tools/list`、private Inbox long-poll 和依赖 watcher 可能同时执行。不要假设“上一个 Tool 一定已经返回，下一个才会进来”。如果某块业务状态必须串行，请只给那块状态加锁。

不需要、也不能在 `tools.yaml` 里声明 `parallel: true`；并发是 Package V2 MCP runtime 的基础协议语义，不是每个 Plugin 自选的优化项。

## 4. tools.yaml：MCP public Tool 与 private Inbox

MCP runtime 必须提供 `tools.yaml`：

```yaml
tools:
  - demo_context
  - demo_run
inbox: demo_inbox
```

原则：

- `tools` 是 Host-visible allowlist，不复制 Tool input schema；
- 真实 Tool schema 来自 live runtime `tools/list`；
- `inbox` 是 private Plugin endpoint，不放进普通 Tool allowlist；
- Tool/Inbox handler 要能安全并发，尤其不能让长轮询占住整个 runtime；
- Native runtime 不写 `tools.yaml`。

模型通过稳定的 `plugin_tool` 调用 Plugin，不会因为安装新 Plugin 而动态增加 Host Tool 名。

## 5. Private Inbox

如果 Plugin 有异步工作，可以实现自己的 private Inbox。MCPX 通过 `plugin_tool.inbox` 聚合所有 active Plugin inbox，因此 Plugin 不应要求主模型直接调用自己的 Inbox Tool。

Plugin 自己负责：

- cursor；
- immediate/deferred/retention 语义；
- 业务事件去重；
- 哪些变化真的值得 owner attention；
- 可选 taxonomy。

尽量使用“新事件/状态边沿”触发 immediate，不要持续重复“我还是坏的”。

## 6. Taxonomy：只在特殊 workflow 路口使用

Taxonomy 是可选动态引导，不是每个 Plugin 的必选字段。

适合：

> “当前长任务仍正常工作；Inbox 已安静等待 25 秒，请再耐心等约 35 秒再轮询，不要主动催促。”

不适合：

- 复制 Tool usage manual；
- 暴露 reasoning；
- 建一套完整 declarative workflow engine；
- 让 `plugin_tool` 根据事件自己猜下一步。

如果 private Inbox 返回 taxonomy，MCPX 聚合时保留 Plugin 身份并原样转发。

## 7. Guidance：渐进式披露

需要稳定模型规则时，在 `plugin.yaml` 声明：

```yaml
guidance: ./guidance
```

每条 Guidance 一个小 Markdown 文件：

```markdown
---
id: demo.waiting
summary: Demo 长任务等待规则
scope: context
---
若任务仍正常推进，请保持等待……
```

要求：

- `id / summary / scope` 必填；
- summary 保持很短，用于 metadata discovery；
- 正文非空；
- 一条 Guidance 对应一个明确场景；
- 人类架构说明放 `docs/`，不要塞进 Guidance。

MCPX `guidance_list` 只给 metadata/revision；只有 `guidance_bind` 才披露正文。目标是**需要时才递一张路牌**，不是启动时把所有说明书塞进上下文。

## 8. requires / uses / watches

依赖：

```yaml
requires:
  plugins: [Comet, JEA]
```

受限 private mount：

```yaml
uses:
  agent_status:
    plugin: JEA
    tool: agent_status
    automatic: true
    constraints:
      path:
        prefix: /agents/demo-
```

String guard 支持：

```yaml
constraints:
  action: { equals: status }
  path: { prefix: /agents/demo- }
  type: { one_of: [explore, review] }
```

Watch attention source：

```yaml
watches:
  - plugin: JEA
    source: inbox
    scope: sessions
```

这些字段声明 integration boundary，不把依赖 Plugin 的业务状态机复制进当前 Plugin。

## 9. Skill Injection

Target 显式接受：

```yaml
skill_injection:
  accepts:
    - slot: eval.subject.guidance
      skill: demo-any
      max_bytes: 1024
```

Provider 显式贡献：

```yaml
skill_injection:
  provides:
    - plugin: Target
      slot: eval.subject.guidance
      path: ./skills/subject.md
```

Skill Injection 是 Plugin-to-Plugin 内容合同，不是 Guidance 的别名。

## 10. TUI

```yaml
ui:
  tui:
    title: Demo
    command: node
    args: [./src/cli.js, tui]
    env: {}
```

TUI 是展示/交互 client，不拥有 business runtime。Launcher 可以托管它的 PTY；页面退出不应意味着 Plugin process 退出。

## 11. Validate / inspect / install

开发时先验证：

```bash
mcpx plugin validate ./plugin.yaml
mcpx plugin inspect --json ./plugin.yaml
```

也可以传 package 目录：

```bash
mcpx plugin validate .
mcpx plugin inspect --json .
```

Global install/update：

```bash
mcpx plugin install ./plugin.yaml
mcpx plugin update ./plugin.yaml
```

Workspace activation：

```bash
mcpx plugin activate --workspace my-project Demo
mcpx plugin status --workspace my-project --json
mcpx plugin deactivate --workspace my-project Demo
```

不要让 Launcher/Plugin 自己再实现 parser；需要 normalized definition 就消费 `plugin inspect --json`。

## 12. Workspace 与 lifecycle

Workspace-scoped Plugin 运行时以稳定 `workspace_id` 隔离。同一 Workspace 中不同 Remote Session 可以共享 business runtime，但 Session 自身仍按 Remote Session ID 绑定业务状态。

Remote Session 不负责 MCPX 进程保活。Launcher/Workbench 是常驻所有权；request/task/operation 是临时 owner。

Plugin lifecycle cleanup 应可并发安全复用同一个 completion，避免多个 close/shutdown caller 产生孤儿进程或重复释放。若实现使用 MCPX 提供的 lifecycle integration，generation 是当前 managed lease 的身份：ready 后失去 control authority 应清理并退出，不应把旧代进程长期重连成 orphan。

业务实现也要按“请求可并发”设计：读操作尽量无锁/细粒度锁，写操作只串行化真正共享的状态；不要用一把全局锁把 Inbox long-poll、schema 读取和无关 Tool call 全部串成单车道。Host 在复用 managed MCP Plugin lease 时会重新读取 live `tools/list`，因此运行时 schema 应始终代表当前进程真实能力。

## 13. 作者检查清单

- [ ] `plugin.yaml` 只放 identity/index/integration graph；
- [ ] runtime 放 `runtime.yaml`；
- [ ] MCP Tool allowlist/private Inbox 放 `tools.yaml`；
- [ ] 没有复制 live Tool schema；
- [ ] 没有公开 private Inbox；
- [ ] taxonomy 只用于具体 workflow 动态纠偏；
- [ ] Guidance 小而场景化，metadata-first；
- [ ] 人类文档与模型 Guidance 分离；
- [ ] 没有手写 normalized `.mcp.json` Plugin shape；
- [ ] `mcpx plugin validate ./plugin.yaml` 通过；
- [ ] `mcpx plugin inspect --json ./plugin.yaml` 能得到预期 resolved definition；
- [ ] 同一 Plugin connection 上两个独立请求可并发在途，长 Inbox 不阻塞无关 Tool；
- [ ] runtime/TUI/Inbox lifecycle 有测试，generation/ownership 丢失不会留下 orphan；
- [ ] Workspace 隔离使用稳定 identity，不把历史 path 当身份。
