# MCPX Plugin Developer Contract — Package V2

> Status: normative for third-party Plugin authors.
>
> Package V2 is the only supported author format. Normalized `.mcp.json`, runtime lease files, internal Go types, and Launcher state are not author APIs.

## 1. Contract layers

| Layer | Required | Purpose |
|---|---:|---|
| `plugin.yaml` | yes | identity, component index, static integration graph |
| `runtime.yaml` | yes | MCP/native process contract |
| `tools.yaml` | MCP only | Host tool allowlist + private Inbox endpoint |
| `guidance/*.md` | no | progressive model Guidance |
| Native lifecycle | native runtime | MCPX-owned ready/shutdown/private integration |
| `requires/uses/watches` | no | cross-Plugin integration boundary |
| Skill Injection | no | bounded Plugin-to-Plugin instruction contribution |
| TUI | no | human terminal page, independent of business runtime ownership |

## 2. Package entry

MCPX accepts either a package directory or its canonical `plugin.yaml`:

```bash
mcpx plugin validate ./plugin.yaml
mcpx plugin validate .
mcpx plugin inspect --json ./plugin.yaml
```

A different single-file entrypoint is invalid.

Minimal `plugin.yaml`:

```yaml
manifest_version: 2
name: Demo
description: Demo Plugin
runtime: ./runtime.yaml
tools: ./tools.yaml
```

Strict YAML decoding is part of the contract: unknown fields, duplicate/multiple YAML documents, missing required fields, or unsupported runtime types fail validation.

## 3. plugin.yaml fields

Supported top-level author concepts are:

```text
manifest_version = 2
name
description?
runtime
tools?
guidance?
requires?
uses?
watches?
skill_injection?
ui?
```

`plugin.yaml` must not contain normalized MCPX state such as `enabled`, `trust`, `isPlugin`, `server`, runtime lease identity, or copied live Tool schemas.

`runtime` points to `runtime.yaml`. `tools` points to `tools.yaml` and is required for MCP runtime, forbidden for Native runtime. `guidance` points to a directory of Markdown Guidance assets.

## 4. runtime.yaml

```yaml
type: mcp | native
scope: workspace
command: node
args: [./src/cli.js, plugin]
env: {}
```

MCPX owns runtime process lifecycle and resolved Workspace binding. Business labels such as coordinator/watcher are not runtime types.

Relative path-like command/argument values are resolved from the Package V2 root. Ordinary executable names remain PATH-resolved.

For `type: mcp`, the managed Plugin connection is **multiplexed by contract**. MCPX may keep multiple JSON-RPC requests concurrently in flight on the same long-lived stdio connection, including `tools/list`, private Inbox waits, dependency watches, and business Tool calls. Plugin authors must not rely on Host-side request serialization. If a specific business mutation requires ordering, the Plugin must serialize that state internally rather than blocking the entire transport. MCPX refreshes live `tools/list` when reusing a managed lease, so connection health and schema authority are checked on the active runtime rather than only from a startup snapshot.

## 5. MCP tools.yaml

```yaml
tools:
  - demo_context
  - demo_run
inbox: demo_inbox
```

Contract:

- `tools` is an allowlist only;
- live runtime `tools/list` is authoritative for current schemas;
- `inbox` is private and must not also be exposed as a normal Host Tool;
- Host calls Plugin business tools only through stable `plugin_tool` discovery/describe/call;
- MCP runtime handlers must tolerate concurrent requests on one process/connection;
- a long-polling private Inbox must not require exclusive ownership of the Plugin transport.

Schema drift is handled at runtime; Package V2 does not snapshot full Tool schemas.

## 6. Native runtime

A Native Plugin is an MCPX-managed Workspace process using the MCPX private native lifecycle/protocol. The public author contract is the Package V2 declaration, not the wire implementation details.

Native Plugin integration may use `requires`, `uses`, `watches`, Guidance, Skill Injection, owner signals, and TUI. MCPX remains the authority for runtime lease, Workspace identity, target schema checks, and safety constraints.

## 7. requires

```yaml
requires:
  plugins: [Comet, JEA]
```

Dependencies are names in the same effective Workspace Plugin graph. Self-dependencies and invalid/cyclic graphs are rejected by MCPX validation.

Cross-Plugin `uses`, `watches`, and Skill Injection targets must remain inside the declared dependency boundary required by the normalized graph contract.

## 8. uses

```yaml
uses:
  agent_status:
    plugin: JEA
    tool: agent_status
    automatic: true
    constraints:
      path: { prefix: /agents/demo- }
      type: { one_of: [explore, review] }
```

Each string constraint supports one of:

```text
equals
prefix
one_of
```

`automatic: true` does not bypass target Tool allowlists, current live schema, Workspace/Session routing, target permission logic, or upstream safety.

The local alias is private integration vocabulary; it does not create a new Host Tool.

## 9. watches

```yaml
watches:
  - plugin: JEA
    source: inbox
    scope: sessions
```

A watch consumes the target Plugin attention channel according to MCPX Native integration. It is not a generic event bus and does not expose another Plugin's private storage/protocol.

## 10. Private Inbox

Inbox is a Plugin framework capability implemented privately by each participating Plugin and aggregated by `plugin_tool.inbox`.

A Plugin owns its event admission, cursor, retention/delivery classification, deduplication, and optional taxonomy. MCPX aggregation preserves source identity and isolates source failures; it does not infer Plugin business semantics.

Important events from any Plugin may wake the owner. Producers should prefer edge/new-event semantics over repeatedly emitting the same unchanged failure as immediate.

## 11. Taxonomy

Taxonomy is an **optional workflow-specific guidance payload** returned by a Plugin at a moment where the owner model is likely to make a predictable mistake.

It is not a universal state machine, `next_action` protocol, or reasoning channel. `plugin_tool` forwards it with Plugin identity; it does not synthesize the guidance.

Taxonomy should be short and current. Stable instructions belong in Guidance.

## 12. Guidance

`plugin.yaml` may reference a Guidance directory:

```yaml
guidance: ./guidance
```

Each `*.md` must contain YAML front matter and a non-empty body:

```markdown
---
id: demo.waiting
summary: Demo long-running wait rule
scope: context
---
Body shown only after an explicit bind.
```

Required front-matter fields:

```text
id
summary
scope
```

MCPX exposes compact metadata/revision through Guidance discovery. The body is disclosed only through binding to a consumer according to scope/revision rules. Authors should keep Guidance small and scenario-specific; general human architecture documentation belongs under `docs/`.

Guidance must never be treated as a place to dump all Plugin documentation into every model context.

## 13. Skill Injection

Target accepts a bounded slot:

```yaml
skill_injection:
  accepts:
    - slot: eval.subject.guidance
      skill: demo-any
      max_bytes: 1024
```

Provider contributes content:

```yaml
skill_injection:
  provides:
    - plugin: Target
      slot: eval.subject.guidance
      path: ./skills/subject.md
```

Target opt-in, dependency graph, content bounds, and revision integrity remain MCPX-enforced. Skill Injection is distinct from model Guidance.

## 14. TUI

```yaml
ui:
  tui:
    title: Demo
    command: node
    args: [./src/cli.js, tui]
    env: {}
```

TUI is a human-facing page. It may observe/control the Plugin only through documented runtime interfaces; it is not the business runtime owner. Closing the TUI must not implicitly terminate the Plugin lease.

## 15. Workspace identity and runtime ownership

Workspace-scoped runtime identity is rooted in durable `workspace_id`. Remote Session may persist across MCPX process restarts and resolves its Workspace through Registry identity, not stale saved paths.

Remote Session is not an MCPX residency root. Workbench/Launcher ownership and in-flight request/task/operation roots control process residency. If an external controller replaces the whole MCPX Instance, that controller must reopen its Workbench/root holder on the new Instance; merely reopening a durable Remote Session does not keep the Runtime resident.

Managed Plugin leases are generation-scoped. Runtime-side lifecycle integrations must not let an old generation survive ownership loss as an orphan; stale persisted process identity is a recovery aid, not a public Plugin API.

A Plugin must not create its own second MCPX-managed business runtime to bypass lease ownership.

## 16. Host surface

Plugin installation does not dynamically add arbitrary Host Tool names. The stable Host surface includes Plugin discovery/call/Inbox/Guidance/owner-signal operations under `plugin_tool`.

Plugin business Tool schemas are resolved at use time from the active runtime. Clients must not rely on private MCPX lease files, private sockets, or normalized storage layout as an API.

## 17. Install / update / activation

```bash
mcpx plugin validate ./plugin.yaml
mcpx plugin inspect --json ./plugin.yaml
mcpx plugin install ./plugin.yaml
mcpx plugin update ./plugin.yaml
mcpx plugin activate --workspace my-project Demo
mcpx plugin status --workspace my-project --json
mcpx plugin deactivate --workspace my-project Demo
```

Install/update manage Global normalized definitions. A trusted repo Workbench may bootstrap a Workspace-owned normalized definition only by consuming `plugin inspect --json`; it must not implement another Package V2 parser.

Activation is Workspace state and is not author metadata.

## 18. Trust and safety

Package V2 does not declare `trust: true`. Install/bootstrap is the acceptance boundary.

Trust never bypasses:

- public Tool allowlist;
- current Tool schema;
- `uses` constraints;
- Remote Session / Workspace routing;
- target Plugin/Provider permission and confirmation mechanisms;
- Skill Injection target opt-in/bounds.

## 19. Compatibility rule

Package V2 is the sole author contract. Implementations must reject legacy single-file Plugin manifests rather than silently supporting a second author format.

## 20. Release checklist

- `mcpx plugin validate ./plugin.yaml` succeeds;
- `plugin inspect --json` resolves expected paths/graph/TUI/Guidance;
- MCP Tool allowlist matches the intended public subset without copying schemas;
- private Inbox is not public;
- important Inbox events are deduplicated/meaningful;
- taxonomy, if any, is short workflow guidance rather than a second state machine;
- Guidance is progressive and compact;
- Tool/Inbox shared state is concurrency-safe and does not assume Host request ordering;
- runtime close/shutdown is concurrency-safe;
- Workspace isolation is keyed by stable identity;
- TUI exit does not own business runtime shutdown;
- no code depends on normalized `.mcp.json`, lease files, or another Plugin's private protocol as a public API.
