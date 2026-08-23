# MCPX Native Plugin Runtime Handoff

本文描述 `runtime.type = "native"` 的 MCPX 私有运行协议与 Host ownership。作者 Manifest 结构见 [`PLUGIN_AUTHORING.md`](./PLUGIN_AUTHORING.md)，总体边界见 [`PLUGIN_ARCHITECTURE.md`](./PLUGIN_ARCHITECTURE.md)。

## 1. 定位

Native Plugin 是 MCPX 托管的 Workspace-local process。它不是 MCP Server，也不是某种固定业务角色。

```text
CometCoordinator  → Native Plugin
PolicyGuard       → Native Plugin
WorkspaceWatcher  → Native Plugin
```

业务上可以叫 Coordinator/Watcher/Manager；平台 runtime 统一叫 **Native**。

V1：

```json
{
  "runtime": {
    "type": "native",
    "scope": "workspace",
    "command": "node",
    "args": ["./src/cli.js", "runtime"]
  }
}
```

Native runtime 不声明 MCP `capabilities.tools` / `capabilities.inbox`。Host 直接提供 dependency use、watch、Inbox 与 owner signal 通路。

## 2. Native Runtime Protocol

MCPX 与 Native process 使用逐行 JSON 私有协议：

```text
mcpx-native-v1
```

启动后 MCPX 发送 `init`：

```json
{
  "type": "init",
  "protocol": "mcpx-native-v1",
  "plugin": "DemoCoordinator",
  "workspace": {
    "id": "...",
    "name": "demo",
    "path": "/workspace/demo"
  },
  "runtime_dir": "/.../.mcpx/runtime/plugins/DemoCoordinator/...",
  "depends": ["Demo", "JEA"],
  "mounts": {},
  "subscriptions": []
}
```

`mounts/subscriptions` 是当前内部 normalized wire 名；Author Manifest 使用 `uses/watches`。Native process 不应反向把这些内部字段当成作者合同。

Process ready：

```json
{ "type": "ready" }
```

Host request/response 使用 request ID 关联；Native process 不能猜测或重建 Host request identity。

## 3. `requires`

Author Manifest：

```json
"requires": {
  "plugins": ["Comet", "JEA"]
}
```

MCPX 在 Native runtime 启动前按 effective Workspace graph 确保依赖合法并建立 lease holder。任何依赖失败都会回滚本候选 runtime 已取得的 holder，不留下半启动 graph。

Dependency runtime revision 变化会使依赖它的 Native runtime 失效，并在下一次 ensure 使用新 graph。

## 4. `uses`

Author Manifest：

```json
"uses": {
  "agent_spawn": {
    "plugin": "JEA",
    "tool": "agent_spawn",
    "automatic": true,
    "constraints": {
      "path": { "prefix": "/agents/demo-" }
    }
  }
}
```

Native process 通过 Host request 请求一个已声明 use alias。MCPX 每次调用时检查：

1. target 已在 `requires.plugins`；
2. target 是当前 effective MCP Plugin；
3. Tool 在 target allowlist；
4. 当前 `tools/list` 返回 Tool；
5. 参数满足 manifest constraints；
6. 当前 Tool schema 校验通过；
7. 上游 permission / approval / safety 继续生效。

`automatic=false` 的 use 不能被 Native process 当作自动 capability 调用。

## 5. `watches`

Author Manifest：

```json
"watches": [
  { "plugin": "JEA", "source": "inbox", "scope": "sessions" }
]
```

V1 Host 只投递 dependency Plugin 已经 admission 到其 Inbox attention channel 的消息，不复制完整内部 timeline。

`scope=sessions` 时，只有与当前 Native runtime 已 attach Remote Session 相关的消息携带该 Session identity。

## 6. Native Inbox V3

Native process 可以向 Host emit attention event：

```json
{
  "type": "emit",
  "event": {
    "kind": "review_completed",
    "delivery": "deferred",
    "action_required": false,
    "summary": "Review completed"
  }
}
```

Host 验证：

```text
delivery = immediate | deferred
action_required=true => immediate
```

`delivery=drop` 可以作为 Native process 内部 admission decision，但 Host 不持久化该 event。

Native Inbox 和 MCP Plugin Inbox 最终都由 `plugin_tool(inbox)` 聚合为统一 V3 attention items。

## 7. Owner Signal

当 Native event 使用：

```text
delivery=immediate
action_required=true
```

Parent Agent 可以处理 owner/user gate，并通过稳定 Host surface 返回决定：

```text
plugin_tool(action="signal", plugin="DemoCoordinator", signal="...", data={...})
```

MCPX 验证 Remote Session/Workspace identity 后向 Native process 投递 `owner.signal`。Native Plugin 不需要额外暴露 MCP Tool。

## 8. Guidance 与 Skill Injection

这两个能力独立于 Native protocol：

```text
Guidance        Plugin → Model Context
Skill Injection Plugin → another Plugin's Skill instructions
```

Native Plugin 可以声明 Attachment/Context Guidance，也可以静态 `skill_injection.provides`。这些内容由 MCPX 从 trusted manifest asset 读取、限长并 revision/hash；不要求 Native process 自己传输文本。

## 9. Lifecycle

MCPX 是 Native business process 的 lifecycle authority：

- Workspace 一份 canonical lease；
- cwd = Workspace root；
- state root = `MCPX_PLUGIN_RUNTIME_DIR`；
- MCPX 管理 process identity、runtime revision 和 lifecycle peer；
- Launcher/TUI 不启动第二份 business runtime；
- TUI page lifecycle 与 Native business runtime lifecycle 分离。

Native process 的 graceful shutdown/EOF 规则由 MCPX lifecycle protocol 管理；cleanup 失败不能伪装成 graceful completion。

## 10. 设计不变量

1. Native 是 runtime type，不是业务角色。
2. Author Manifest 用 `requires/uses/watches`，私有 wire 可以保持 normalized internal 字段。
3. Native runtime 不扩展 Host `tools/list`。
4. Automatic dependency use 永远受 allowlist/schema/constraints/upstream safety 限制。
5. Watches 只消费 attention Inbox，不等价于全量 event bus。
6. Owner gate 必须经过 Remote Session identity 验证。
7. MCPX 管 business runtime；Launcher 只管 human TUI page process。
