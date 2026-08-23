# MCPX Plugin Architecture

MCPX Plugin 是由 MCPX 管理 definition、Workspace activation、runtime lifecycle、integration boundary、Guidance、Inbox 与 TUI 的扩展单元。作者只维护 **Package V2**；`.mcp.json` 仍是 MCPX normalized internal state，不是作者格式。

作者上手见 [`PLUGIN_AUTHORING.md`](./PLUGIN_AUTHORING.md)，可依赖的稳定接口见 [`PLUGIN_DEVELOPER_CONTRACT.md`](./PLUGIN_DEVELOPER_CONTRACT.md)。

## 1. Ownership

```text
Plugin Package V2
  plugin.yaml
  runtime.yaml
  tools.yaml?          (MCP runtime only)
  guidance/*.md?       (progressive disclosure)
        │
        │ mcpx plugin validate / inspect
        ▼
┌─────────────────────────────────────────────┐
│ MCPX                                        │
│                                             │
│ normalized Plugin definition               │
│ Workspace activation / dependency graph     │
│ Workspace-scoped runtime leases             │
│ private Inbox aggregation                   │
│ Guidance discovery / binding                │
│ Skill Injection / Native uses+watches       │
│ resolved TUI page registry                  │
└─────────────────┬───────────────────────────┘
                  │
          ┌───────┴────────┐
          ▼                ▼
      MCP runtime      Native runtime
```

MCPX 是 Plugin business runtime lifecycle authority。Launcher、TUI、Agent 或 Plugin 自己都不应绕过 MCPX 启动第二份受管 runtime。

Plugin 仍拥有自己的业务状态机。MCPX 管理扩展边界，不复制 JEA Agent、Comet workflow 或其他 Plugin 的业务语义。

## 2. Package V2 与 internal definition

`plugin.yaml` 是 package index，只声明身份和组件位置，以及确实属于跨 Plugin 集成图的静态关系：

```yaml
manifest_version: 2
name: Example
description: Example workspace Plugin
runtime: ./runtime.yaml
tools: ./tools.yaml       # MCP runtime only
guidance: ./guidance      # optional directory
requires:
  plugins: [Other]
uses: {}
watches: []
skill_injection: {}
ui: {}
```

`runtime.yaml` 只回答“怎么运行”：

```yaml
type: mcp                # or native
scope: workspace
command: node
args: [./src/cli.js, plugin]
env: {}
```

MCP runtime 通过 `tools.yaml` 声明 Host 可调用工具和 private Inbox endpoint：

```yaml
tools:
  - context
  - status
inbox: inbox
```

`.mcp.json` 中的 `MCPServer/MCPPlugin` 是 normalized internal definition。作者不直接维护 `server.plugin`、`isPlugin`、`enabled`、`trust`、lease revision 等内部字段。

MCPX 使用 strict YAML decoding；未知字段失败。相对 runtime/TUI/contribution/Guidance 路径以 package 根目录为基准解析。Launcher 只调用 `mcpx plugin inspect --json`，不维护第二份 parser/path resolver。

## 3. Runtime 类型

### 3.1 MCP runtime

MCP Plugin 是标准 MCP process。`tools.yaml.tools` 是 Host-visible allowlist；private Inbox 名不出现在普通 Tool allowlist 中。

MCPX 把 Package V2 MCP Plugin runtime 当作 **multiplexed（多路复用）长连接**：同一 stdio connection 上允许多个 JSON-RPC request 同时在途。`tools/list`、业务 Tool、全局 Inbox 和 Native Controller dependency watch 不应因为共享一条 Plugin connection 而互相排队。若某段业务状态必须串行，锁应放在 Plugin 自己的那段状态上，而不是锁住整个 MCP transport。

普通 MCP Server 不自动获得这个 Plugin runtime 语义；它仍可由 MCPX 以串行会话承载 progress 等 session-level 状态。

模型通过稳定 Host surface：

```text
plugin_tool(list)
plugin_tool(describe)
plugin_tool(call)
plugin_tool(inbox)
```

`plugin_tool` 不复制 Plugin Tool schema；运行时 `tools/list` 仍是动态 schema 的权威来源。

### 3.2 Native runtime

Native Plugin 是 MCPX 管理的 Workspace process，通过私有 native lifecycle/protocol 工作。业务名字如 Coordinator、Watcher 不成为新的 runtime 类型。

Native runtime 可以使用静态 `requires / uses / watches` 集成关系。MCPX 在每次 private mount 调用时仍检查目标 Tool 当前 schema、allowlist、constraints 与上游安全边界。

## 4. Workspace scope 与 lease

Workspace Plugin runtime identity 以稳定 `workspace_id` 为作用域，而不是历史 path：

```text
workspace:<workspace_id>:<plugin>
```

同一 Workspace 中多个 Remote Session 可以共享一个 Workspace-scoped Plugin runtime；不同 Workspace ID 不共享业务进程状态。每次 managed lease 启动都会获得新的 generation；生命周期握手与关闭只属于该 generation，旧代进程失去 MCPX control authority 后应退出，MCPX 也会用持久化进程身份做 stale recovery 兜底。

Remote Session 是 durable business state，不是 MCPX Instance residency root。Workbench/Launcher、request、task、operation 等 runtime owner 决定 Instance residency。whole-runtime replacement 后，外部 owner 必须在新 Instance 上重新获取 Workbench/root holder；一次 Session 请求只提供临时 request holder。

Workspace rename/path migration 通过 Registry 的稳定 ID 重新解析；runtime 不应把 Session 保存的旧 path 当 active fallback。

## 5. Private Inbox 与 plugin_tool.inbox

Inbox 是 **Plugin framework 能力**，不是 JEA 专属机制。

每个实现 Inbox 的 Plugin 自己拥有 private endpoint、cursor、事件去重/重要性判断与业务 taxonomy。private Inbox 不直接暴露给 Host。`plugin_tool.inbox` 并发读取当前有效 Plugin inbox，并把事件统一转发给 owner。

```text
Plugin A private inbox ─┐
Plugin B private inbox ─┼─> plugin_tool.inbox ─> owner model
Plugin C private inbox ─┘
```

聚合层负责：

- Plugin failure isolation；
- cursor 聚合；
- event 来源身份；
- top-level optional taxonomy 透传。

聚合层**不理解业务内容**，不替 Plugin 猜测等待时间、去重语义或 workflow transition。

任何 Plugin 的 important/immediate event 都可能提前唤醒 owner。Plugin 自己应避免把持续相同状态反复当 immediate；更合适的语义是状态边沿或新的 owner-attention event。

## 6. Taxonomy

Taxonomy 是**可选的 Plugin workflow 动态引导**。

它不是：

- 所有 Plugin 强制实现的状态机；
- 通用 `next_action` protocol；
- Agent reasoning 或 reasoning summary；
- `plugin_tool` 自己推导的行为。

它适用于某个 Plugin 知道“主模型在这个路口很容易做错”的场景。比如长时间 JEA Agent 等待后，Plugin 可以短暂提示 owner 不要高频轮询、应再耐心等待。

`plugin_tool` 只保留 `provider_plugin` 身份并原样转发多个 taxonomy，不拍平成一条全局指令。

## 7. Guidance：渐进式披露

Guidance 是稳定使用规则，不是每次调用都重复的 prompt。

Package V2 可声明：

```yaml
guidance: ./guidance
```

目录下每个 `*.md` 是独立小 Guidance：

```markdown
---
id: example.waiting
summary: 长任务等待规则
scope: context
---
正文……
```

front matter 必须包含 `id / summary / scope`，正文非空。`guidance_list` 只暴露 compact metadata/revision；只有显式 `guidance_bind` 才向特定 consumer 披露正文，并避免同一 consumer 反复占用上下文。

职责边界：

```text
Tool schema  -> 能调用什么、参数是什么
Inbox        -> 有新的异步事情需要 owner 注意
Taxonomy     -> 当前 workflow 路口的短期动态引导
Guidance     -> 第一次需要时才披露的稳定使用规则
```

Tool description 与 taxonomy 不应复制整份 Guidance。

## 8. Skill Injection

Skill Injection 是声明式、受限的跨 Plugin 内容贡献。Provider 可以提供 contribution；Target 必须显式接受 slot/skill/大小约束。MCPX 负责 resolved graph 与安全校验，Plugin 不直接写另一个 Plugin 的内部文件。

它与 Guidance 不同：Guidance 面向模型运行时的使用规则；Skill Injection 是 Plugin-to-Plugin 的内容贡献合同。

## 9. TUI

Package V2 可声明一个 full-screen TUI：

```yaml
ui:
  tui:
    title: Example
    command: node
    args: [./src/cli.js, tui]
```

MCPX 解析 launch spec；Launcher 只管理 PTY/page navigation。TUI 不拥有 business runtime，不应因为页面退出而停止 Plugin process。

## 10. Launcher 边界

Launcher 可以在 trusted repo root 发现：

```text
plugins/*/plugin.yaml
```

然后调用 MCPX inspect 并原子镜像 resolved definition 到当前 Workspace `.mcpx/.mcp.json`。Launcher 不：

- 解析 Package V2；
- 打开 Plugin private business protocol；
- 直接启动/重启 Plugin business runtime；
- 写用户级 Global Plugin store；
- 把一个 Plugin 的业务状态机复制到 UI。

一个 Launcher 同一时刻只有一个 active Workspace Workbench，但同一 MCPX Instance 可以服务多个 Workspace ID/多个 Launcher。Launcher 自己发起 Runtime restart 时必须执行 `restart → Attach Workspace → OpenWorkbench` 完成交接；外部 CLI 直接替换 Instance 不会自动继承旧 lifecycle socket 上的 holder。

## 11. Trust 与安全边界

Package V2 不自我声明 `trust: true`。安装或 trusted Workspace bootstrap 是接受边界。

即便 Plugin definition 被信任，也不能绕过：

- Host Tool allowlist；
- 动态 Tool schema；
- Native `uses` constraints；
- Remote Session / Workspace routing；
- 上游 Provider/Plugin 自己的权限与确认机制。

## 12. 设计原则

1. Package V2 是唯一作者格式；normalized `.mcp.json` 不是作者 API。
2. `mcp/native` 只描述 runtime，不描述业务角色。
3. MCPX 管 business runtime 与跨 Plugin 边界，Plugin 管自己的业务状态。
4. Workspace identity 用稳定 `workspace_id`，不以历史 path 充当身份。
5. MCP Plugin transport 原生多路复用；业务需要顺序时由 Plugin 只锁自己的状态，不锁整条连接。
6. private Inbox 由各 Plugin 实现，统一经 `plugin_tool.inbox` 聚合。
7. taxonomy 是可选的动态 workflow 引导，不升级成第二套 orchestration engine。
8. Guidance 以 metadata-first + bind-on-demand 实现渐进式披露。
9. Launcher authority-light：做 bootstrap、导航和 PTY，不做 Plugin runtime manager。
