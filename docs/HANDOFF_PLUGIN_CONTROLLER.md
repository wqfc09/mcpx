# MCPX Plugin Controller V1 Handoff

## 目标

Plugin 不再等同于 MCP Server。MCPX 把 Plugin 定义为由 Instance 托管生命周期、Global 合同和 Workspace 激活的可信扩展单元；MCP 只是 Plugin 的一种 runtime。

V1 支持：

- `runtime=mcp`：提供稳定 public tools、private inbox，并可声明接受的 Skill contribution slots。
- `runtime=controller`：Workspace-scoped 本地 sidecar，不讲 MCP；负责依赖、受限能力调用、事件订阅、短 guidance contribution、durable inbox 和 owner gate。

核心原则：原生能力尽量交给原生实现。Controller 只保存关联/协调状态，不复制依赖 Plugin 的业务状态机。

## Global definition 与 Workspace activation

Plugin 身份、schema、runtime、依赖、mount、subscription、contribution 都只来自 Global registration。Workspace 同名 overlay 只能启用/禁用已有 Plugin，不能授予 Plugin 身份或重定义能力。

Controller 不隐藏激活依赖。若 `Coordinator` 声明：

```json
{
  "depends": ["Comet", "JEA"]
}
```

则目标 Workspace 必须显式启用 `Comet`、`JEA` 和 `Coordinator`。MCPX 不会因为启用 Controller 而偷偷启用依赖。

## Runtime contract

### MCP runtime

```json
{
  "runtime": "mcp",
  "scope": "workspace",
  "tools": ["comet_context"],
  "inbox": "comet_inbox",
  "accepts": [
    {
      "slot": "creator.reviewer.guidance",
      "skill": "comet-any",
      "max_bytes": 1024
    }
  ]
}
```

V1 的 MCP runtime 不声明 Controller orchestration 字段：`depends`、`mounts`、`subscriptions`、`contributes`。如果以后出现真正的 MCP→Plugin runtime dependency 场景，应单独提升为跨 runtime 依赖语义，而不是让配置先于运行时能力。

### Controller runtime

```json
{
  "runtime": "controller",
  "scope": "workspace",
  "tools": [],
  "inbox": "",
  "depends": ["Comet", "JEA"],
  "mounts": {},
  "subscriptions": [],
  "contributes": []
}
```

Controller 必须是 Workspace scope。它没有 MCP catalog probe，也不会生成 `plugin.<Controller>.*` public tools。其进程由 MCPX 以 Workspace lease 启动，cwd 为 Workspace root，runtime state 位于 `MCPX_PLUGIN_RUNTIME_DIR`。

## Controller 私有协议

MCPX 与 sidecar 使用逐行 JSON 的 `mcpx-controller-v1` 私有协议。

Host 初始化：

```json
{
  "type": "init",
  "protocol": "mcpx-controller-v1",
  "plugin": "CometCoordinator",
  "workspace": {"id": "...", "name": "...", "path": "..."},
  "runtime_dir": "...",
  "depends": ["Comet", "JEA"],
  "mounts": {},
  "subscriptions": []
}
```

Controller 可输出：

- `ready`
- `emit`：写入 MCPX-hosted durable Controller inbox。
- `call`：调用 Global manifest 中已声明且 `automatic=true` 的 mount。

非 automatic mount 会被 Host 拒绝，Controller 应改为向自己的 inbox 发 owner-required 事件。

## depends 与 mounts

`depends` 定义启动/健康依赖；`mounts` 给 Controller 一个稳定 alias，只能引用已声明 dependency 的 public MCP Plugin tool。

```json
{
  "agent_spawn": {
    "plugin": "JEA",
    "tool": "agent_spawn",
    "automatic": true,
    "guards": {
      "path": {"prefix": "/agents/comet-"},
      "sandbox": {"equals": "read-only"},
      "type": {"one_of": ["explore", "implement", "review"]}
    }
  }
}
```

V1 guard 只约束 string 参数，并且每个参数恰好使用一种规则：

- `equals`
- `prefix`
- `one_of`

Guard 由 MCPX Host 在 dependency Tool schema 校验和真正调用之前执行。它是 Global authority 的最小权限合同，不依赖 sidecar 自律。

## subscriptions 与 Event Bus

V1 subscription 只支持 dependency Plugin private inbox，并明确事件作用域：

```json
{"plugin": "Comet", "kind": "inbox", "scope": "workspace"}
{"plugin": "JEA", "kind": "inbox", "scope": "sessions"}
```

`scope` 省略时默认 `workspace`。

- `workspace`：一个 Workspace 级 cursor，适合 Comet 这类 Workflow Inbox。
- `sessions`：MCPX 为 Controller 当前每个 attached Remote Session 分别维护 cursor，并把已验证的 `remote_session_id` 透传到 dependency MCP `_meta`；事件 source 也带同一 Session ID，适合 JEA 这类 Session-scoped Inbox。

MCPX long-poll dependency inbox，只在 `structured_content.items` 非空时向 Controller 投递事件。立即返回的空 page 会做短 backoff，避免忙循环。

MCPX 还投递 Controller 生命周期事件：

- `session.opened`
- `session.closed`

Remote Session attachment 按稳定 Workspace ID 验证。Controller 发 automatic mount call 时可以携带已 attachment 的 `remote_session_id`；MCPX 校验后才把它放入 dependency MCP CallTool `_meta["mcpx/remote_session_id"]`。这允许 JEA 这类 session-scoped Plugin 被安全自动调度，Controller 不能伪造或跨 Workspace 使用 Session。

## Controller durable inbox

Controller 自身没有 MCP inbox tool。MCPX 在其 runtime dir 维护：

```text
inbox.jsonl
```

每条记录有单调 `seq`，对外通过现有 `plugin_tool(action="inbox")` 聚合，使用 decimal cursor 和 long-poll。Controller 重启不会丢失已经发出的协调事件。

## Owner signal

主模型需要在硬 gate 处做最终裁决时，使用现有 `plugin_tool`：

```json
{
  "action": "signal",
  "remote_session_id": "...",
  "purpose": "approve Creator review",
  "plugin": "CometCoordinator",
  "signal": "creator.owner_decision",
  "data": {
    "creator": "demo-skill",
    "decision": "approve"
  }
}
```

`signal` 只允许目标为 active Controller runtime。MCPX 校验当前 Remote Session/Workspace 后投递 `owner.signal` 事件；Controller 仍不需要暴露 MCP tool surface。

这形成完整闭环：

```text
observe -> automatic guarded work -> waiting_owner -> plugin_tool(signal) -> continue
```

## Skill contribution

Controller 可向 dependency MCP Plugin 的 accepted slot 提供短 guidance：

```json
{
  "plugin": "Comet",
  "slot": "creator.reviewer.guidance",
  "path": "/absolute/plugin/assets/reviewer.md"
}
```

规则：

- contribution asset 来自可信 Global Plugin definition，路径必须绝对。
- 全局硬上限 `4096` bytes；目标 slot 可以进一步缩小。
- 目标 MCP Plugin 必须显式 `accepts` 该 slot，并指定可选 `skill` 名称。
- MCPX 不修改 Workspace 中 upstream Skill 文件。
- active contributions 以 source/slot/revision 排序，内容做 SHA-256。
- `skill_tool(describe/call)` 在原生 Skill 尾部追加带 source/slot/revision 的 overlay。
- contribution revision 纳入 Skill discovery/call revision；describe 后 guidance 变化会得到 `skill_revision_changed`，必须重新 describe。
- 这保证已经返回给模型的一次 Skill instructions 不会在执行中被静默换 policy。

MCP business process也会收到只读 contribution manifest env，便于 context/observability 报告当前 active guidance；实际执行 pinning 仍由 Skill discovery/call revision 负责，不要求因为几十字 guidance 变化重启整个 Plugin lease。

## Skill discovery

所有非绝对、非 `~` Skill discovery root 都相对当前 Workspace。默认包含：

```text
.mcpx/skills
```

因此 Comet 原生：

```bash
comet init --scope project --platform mcpx --workflow native
```

生成的 `.mcpx/skills/comet-native` / `.mcpx/skills/comet-any` 可以被 MCPX 直接发现，不需要复制 upstream Skill。

## 安全边界

Controller V1 明确不做：

- 不隐藏 dependency activation。
- 不任意调用未 mount 能力。
- 不把 owner-gated mount 当 automatic。
- 不绕过 Host argument guards。
- 不复制依赖 Plugin 的业务状态。
- 不覆盖 upstream Skill 文件。
- 不通过 contribution 修改正在执行的 Skill revision。
- 不允许跨 Workspace 使用 Remote Session。

## Comet + JEA 样本

当前第二个真实样本是：

```text
Comet Native/Creator = workflow / contract authority
JEA                  = Agent execution authority
CometCoordinator     = coordination policy
Main model            = final decision authority
```

Coordinator 消费 Comet 原生 `AuthoringPlan` DAG，把 lane 派给 JEA，JEA 结果按 Comet 原生 `AuthoringLaneOutput` 回填 `authoring-record`；最终 `skill-review` 后停在 owner gate。它不重新实现 Creator，也不自动 generate/publish。
