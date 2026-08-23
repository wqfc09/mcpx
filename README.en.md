# MCPX

**An MCP Runtime connecting AI to local development environments.**

MCPX is an **MCP Runtime (gateway)** for development environments. ChatGPT, Claude, Cursor, Grok, and other MCP clients that support Streamable HTTP can use one consistent tool surface to inspect projects, review Unified Diffs, modify source code, run tasks, collect environment information, and call local MCP servers and Skills.

Development state is stored in SQLite-backed Remote Sessions. It is independent of any AI vendor or a single `Mcp-Session-Id`, so different clients can query, authorize, hand off, and continue the same development session.

**Documentation:** [中文（默认）](README.md) · English

## Features

| Area | Description |
| --- | --- |
| **Remote Session** | Persistent SQLite sessions, ACLs, and one-time handoff tokens across clients and transports. |
| **Workspace** | One reusable MCPX Instance registers multiple projects with stable Workspace IDs and isolated Remote Sessions. |
| **Terminal** | Run short commands or persistent tasks, inspect logs and ports, attach, and stop tasks. |
| **Source and Edit** | Read source with SHA-256 revisions, apply atomic create/update/rename edits, preserve file format, and inspect Unified Diffs. |
| **Project Task** | Discover project-defined test, build, and check tasks and parse structured diagnostics. |
| **Environment** | Inspect OS, architecture, kernel, display, container, shell, resources, filesystem, and toolchain. |
| **Extensions** | Proxy ordinary upstream MCP servers, mount first-class MCP Plugins, and discover or execute local Skills. |
| **Security and Audit** | OAuth / Bearer authentication, principals, session ACLs, command and file policies, approvals, and JSONL audit logs. |

## Quick start

### 1. Install

Download the archive for your platform from [Releases](https://github.com/opentokenz/mcpx/releases), or build from source:

```bash
git clone https://github.com/opentokenz/mcpx.git
cd mcpx
go build -o bin/mcpx ./cmd/mcpx-server
```

MCPX requires **Go 1.26.1 or later**; the exact development version is defined by `go.mod`.

### 2. Attach and reuse the default Instance

The normal entry point is simply `mcpx` from a project directory:

```bash
cd /path/to/your/project
./bin/mcpx
```

This attaches the current Workspace to the user-level default MCPX Instance. A healthy Instance is reused; otherwise MCPX starts exactly one background Instance and registers the Workspace in **that Instance's own** durable registry. Concurrent first attaches are serialized by a start lock that is independent of `MCPX_HOME`.

Explicit commands are also available:

```bash
./bin/mcpx attach --name my-app /path/to/your/project
./bin/mcpx ensure
./bin/mcpx status
./bin/mcpx serve                # explicit foreground Runtime
```

The default Instance publishes a stable `instance_id`, PID, actual Home, and local Endpoint through a user-level rendezvous that is independent of `MCPX_HOME`. If shell A starts the Instance with Home A and shell B later uses a different `MCPX_HOME`, B still reuses A and routes Workspace lifecycle changes to A's registry instead of mutating a second config by accident. `MCPX_RUNTIME_DIR` is an advanced/test override for the rendezvous location; `MCPX_PORT` or `MCPX_ADDR` controls the first `ensure/attach` bind.

The first Instance start creates runtime data under its **`~/.mcpx/`** (or configured `MCPX_HOME`). The default endpoint is:

```text
http://127.0.0.1:9090/mcp
```

MCPX provides Streamable HTTP only; legacy HTTP+SSE endpoints are not supported.

Check the version with:

```bash
./bin/mcpx -version
```

## Configuration overview

The global configuration is stored at `~/.mcpx/config.yaml`:

```yaml
server:
  host: 127.0.0.1
  port: 9090

auth:
  # mode: open | bearer | oauth | dual
  mode: ""
  token: "" # Static Bearer token
  oauth:
    password: "" # If empty, generated and printed at startup
    server_url: "" # Public origin, required for web OAuth

security:
  commands:
    # Fallback for commands that match no rule: allow | confirm | deny
    default: allow
    allow:
      - ^ls\b
      - ^git status
    confirm:
      - ^git push
      - ^docker
    deny:
      - ^rm -rf /
  files:
    max_read_bytes: 1048576
    max_patch_files: 20
    max_patch_lines: 2000
    deny:
      - ^\.git/
```

The default command policy is `allow`. A command matched by a `confirm` rule still requires explicit approval through `approval_manage` before execution. Do not expose `open` mode to the public internet; use HTTPS, a strong OAuth password, and least-privilege policies.

### Workspace lifecycle

Workspace registrations live in the default Instance's global `config.yaml`. The CLI manages the running Instance when one exists and falls back to offline local config only when no default Instance is running:

```bash
./bin/mcpx workspace list
./bin/mcpx workspace register /path/to/your/project
./bin/mcpx workspace register --name my-app /path/to/your/project
./bin/mcpx workspace rename my-app app
./bin/mcpx workspace unregister app
./bin/mcpx workspace prune
./bin/mcpx workspace prune --apply
```

`workspace list` reports each registration as `ok`, `missing`, or `invalid`. `prune` is a dry run unless `--apply` is supplied. Neither `unregister` nor `prune --apply` deletes, moves, or modifies Workspace files.

The Runtime does not cache the Workspace registry: listing, name resolution, and new Session creation reload the current Instance config. `mcpx workspace ...` first resolves the user-level Instance rendezvous and edits that Instance's Home, even when the calling shell has a different `MCPX_HOME`. Invalid/unverifiable Instance state is an explicit error rather than a silent fallback to another config. Existing Remote Sessions persist a stable Workspace ID and live-resolve the current Registry path on every use; rename follows the current registration, while unregister fails closed instead of falling back to the stored path. New Sessions require a currently registered `ok` Workspace. Valid Workspace paths are canonicalized physically and hashed into a stable 16-hex Workspace ID, so logical rename does not change runtime identity.

### Instruction context

MCPX has one global natural-language instruction source: `~/.mcpx/system_prompt.md`. Global `AGENTS.md` discovery and the configurable `global_agents_path` are not used. Repository instructions live in the Workspace root and directory-level `AGENTS.md` files.

For a Workspace path, MCPX resolves one live instruction context in this order: global `system_prompt.md`, trusted MCP `initialize.instructions`, Workspace-root `AGENTS.md`, then narrower directory `AGENTS.md` files. Global `system_prompt.md` and Workspace `AGENTS.md` share the same instruction semantics inside the Runtime; only their discovery scope and priority differ.

Each `system_prompt.md` or `AGENTS.md` is limited to 64 KiB, and the default inline instruction-context budget is 256 KiB. SHA values are consistency/revision metadata, not trust approvals. Instruction content is live rather than frozen into a Remote Session. Use `runtime_read(view="instructions")` with optional `id`, `anchor_path`, or `paths` to read or resolve current instructions.

### Upstream MCP configuration

MCPX accepts exactly two MCP configuration sources: Global `~/.mcpx/.mcp.json` and Workspace `<workspace>/.mcpx/.mcp.json`. Global provides shared default definitions; a Workspace may define ordinary MCP registrations and complete Plugin definitions. A same-name ordinary Global MCP remains activation-only from the Workspace. A same-name Global Plugin may either receive an `enabled`-only activation overlay or be explicitly replaced by a complete Workspace Plugin definition that is visible only in that Workspace.

Ordinary MCP registrations and Plugin activation support `enabled`, defaulting to `true`. A disabled ordinary MCP remains visible for inventory/debugging but is not callable. Plugins distinguish **installed / active / running**: a Plugin definition present in the Workspace effective graph is installed (from either Global or Workspace configuration), effective `enabled=true` is active, and MCPX owns the actual business runtime lease. Plugin Tool schemas are read from the current runtime during `plugin_tool describe/call`; definition or schema changes never mutate Host `tools/list` and do not require an MCPX Instance restart.

Global `trust: true` is immediately effective. `trust: true` on a **new ordinary Workspace MCP** is a persistent trust request: the first actual call requires user confirmation, then MCPX stores the approval in `~/.mcpx/mcp-trust.json`. The approval is bound to the canonical Workspace path, registration name, and an internal registration fingerprint. Same-name Global activation entries cannot declare their own trust. A complete Workspace Plugin definition is an explicitly trusted project extension definition and does not use the ordinary MCP trust store; Plugin allowlists, Controller guards, Workspace routing, and upstream permissions still apply.

Ordinary Workspace MCP trust fingerprints cover execution-contract fields such as `type`, `command`, `args`, and `injectInstructions`. Plugins use their own definition/runtime revisions to drive contract observation and lease replacement instead of reusing ordinary MCP trust approvals.

An MCP registration may set `injectInstructions: true` to expose `instructions` returned by the MCP initialize handshake, but automatic inclusion still requires effective trust. A Workspace may therefore request both `trust: true` and `injectInstructions: true`; its instructions remain excluded until trust is approved. Natural-language instruction content itself is not separately fingerprinted or Prompt-approved.

### Plugins

Plugin authors use one strict **Package V2** format: `plugin.yaml` is the identity/component index, `runtime.yaml` describes the business process, MCP Plugins use `tools.yaml` for the Host tool allowlist/private Inbox, and `guidance/*.md` provides progressive model guidance. MCPX alone parses, resolves portable paths, and normalizes packages through `plugin validate/inspect`; a repo-owned Workbench consumes only MCPX-resolved definitions:

```bash
mcpx plugin validate ./plugin.yaml
mcpx plugin inspect --json ./plugin.yaml
mcpx plugin install ./plugin.yaml
mcpx plugin activate --workspace my-project JEA
mcpx plugin status --workspace my-project --json
mcpx plugin update ./plugin.yaml
mcpx plugin deactivate --workspace my-project JEA
```

Plugins use `runtime.type=mcp|native`. MCP Plugins expose only their declared tools while keeping the Plugin Inbox private behind `plugin_tool.inbox`; Native Plugins are MCPX-managed Workspace runtimes that can use `requires/uses/watches`, Guidance, Skill Injection, and owner signals. Business roles such as Coordinator or Watcher are not runtime types. The Host-facing API remains the stable `plugin_tool` surface:

```text
plugin_tool(action="list")
plugin_tool(action="describe", plugin="JEA", tool="agent_spawn")
plugin_tool(action="call", plugin="JEA", tool="agent_spawn", arguments={...})
plugin_tool(action="inbox", ...)
plugin_tool(action="signal", ...)
```

`capabilities.tools` normalizes to the internal allowlist and is not a schema snapshot. MCPX reads the current MCP runtime `tools/list` during describe/call. `plugin_tool(inbox)` uses the V3 attention protocol: a 25-second default window, immediate early wake, deferred deadline delivery, and silent cursor reconnect on an empty timeout.

Plugin lifecycle state is explicitly **installed / active / running**. MCPX owns Workspace/Instance scoped runtime leases and verifies the real child process behind `runtime/plugins/.../lease.json`. Runtime-affecting definition updates reconcile old leases and dependent Native runtimes, while description/TUI-only updates do not terminate a working business runtime.

A Plugin may also contribute one independent full-screen local TUI page. `mcpx tui pages --workspace <name> --json` resolves active Plugin pages only. Launcher folds MCPX Instance/Workspace/Plugin runtime status into its own MCPX Dashboard instead of registering a duplicate `id=mcpx` platform page. Standalone `mcpx tui` remains available for direct runtime-status viewing outside Launcher.

See [`docs/PLUGIN_ARCHITECTURE.md`](docs/PLUGIN_ARCHITECTURE.md), [`docs/PLUGIN_AUTHORING.md`](docs/PLUGIN_AUTHORING.md), [`docs/PLUGIN_DEVELOPER_CONTRACT.md`](docs/PLUGIN_DEVELOPER_CONTRACT.md), and [`docs/HANDOFF_PLUGIN_NATIVE.md`](docs/HANDOFF_PLUGIN_NATIVE.md). Author manifests do not self-declare trust; accepted definitions still cannot bypass allowlists, schema checks, Native `uses` constraints, upstream permissions, or upstream safety controls.

## Client integration

For web clients that support Remote MCP and OAuth, expose MCPX through an HTTPS reverse proxy and configure `auth.mode: oauth` or `dual`, `oauth.password`, and `oauth.server_url`. Add the remote URL ending in `/mcp`; the client can complete dynamic client registration and authorization.

For a local client using a static Bearer token:

```json
{
  "mcpServers": {
    "mcpx": {
      "url": "http://127.0.0.1:9090/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_TOKEN"
      }
    }
  }
}
```

To verify the endpoint, send an MCP `initialize` request rather than relying on a bare `GET`:

```bash
curl -sS -m 5 \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"curl","version":"0.1"}}}' \
  http://127.0.0.1:9090/mcp
```

## Tool surface

`tools/list` is the authoritative source for tool names, descriptions, input schemas, and annotations. The public surface is fixed at 20 tools: 13 core tools and 7 support tools. Plugins never add dynamic Host tools; all Plugin discovery and invocation is routed through `plugin_tool`.

The core tools are `workspace`, `session`, `read`, `edit`, `move_out`, `observe`, `progress`, `execute`, `plan`, `artifact`, `skill_tool`, `mcp_tool`, and `plugin_tool`. The support tools are `operation_batch`, `operation_manage`, `runtime_read`, `environment_read`, `environment`, `screenshot_capture`, and `secret_provide`.

All stateful tools use the full `remote_session_id`. The normal source-edit workflow is:

1. Open or attach to a Remote Session with `session`.
2. Read the relevant file and its SHA-256 revision with `read`.
3. Apply create, update, or rename operations atomically with `edit`; updates should prefer exact, unique replacements.
4. Inspect the resulting changes or full diff with `observe`.
5. Run tests or project tasks with `execute`.

`edit` does not delete files. Deletion, removal, and cleanup use one `move_out` tool with two actions: `move_out(action="prepare")` freezes an explicit manifest without mutating the filesystem; after the user confirms that frozen manifest, `move_out(action="submit")` safely moves the targets to the operating system trash. The prepare result returns an exact `next_action` for submit. The submit branch is intentionally strict and accepts only `action`, `remote_session_id`, and `confirmation_uuid`; the manifest, purpose, workspace, and idempotency key remain server-bound.

`execute` supports simple compound commands joined by `&&`, `||`, and `;`. Before any shell process starts, MCPX splits every segment, evaluates command policy for every segment, and records the structured preflight decision. Any denied segment rejects the entire command; any confirmation-required segment causes one confirmation for the frozen whole command. Only after all segments pass and enabled preflight audit persistence succeeds is the original command passed to the shell once. Pipes, redirections, a single background `&`, newlines, `$()`, and backtick command substitution remain rejected. This is an atomic **policy/audit gate**, not transactional rollback: after the shell starts, normal `&&`/`||` conditional semantics apply and side effects from an already executed segment are not rolled back automatically.

## Security boundaries

- `open` is intended for local use only; use `bearer`, `oauth`, or `dual` for authenticated access.
- Remote Session roles include `viewer`, `editor`, `approver`, and `owner`.
- Secret values are kept in process memory and are not written to SQLite, logs, or the workspace.
- Runtime state, credentials, task logs, and audit logs are stored under `~/.mcpx/` with restricted permissions.
- Never place real tokens, passwords, or secrets in this repository or in command strings.
- Review commands and diffs before approving changes, especially when exposing MCPX beyond localhost.

## Future

- **Presentation**: Improve host capability negotiation so clients can select `diff`, `table`, `tree`, or `diagram` views while retaining a safe text fallback.
- **ARC**: Evolve result types and JSON Schemas compatibly, with version negotiation, error recovery, and consistent action descriptions across clients.
- **Large-result delivery**: Unify paginated and streamed Resource Link delivery for diffs, logs, search results, and artifacts to reduce inline response size.
- **Observability**: Extend trace, latency, and result-classification metrics to diagnose client rendering, approval flows, and task execution.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for the branch, pull request, protected `main`, validation, and release conventions. `main` is the protected branch; changes enter it through a pull request and are not pushed directly.

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
test -z "$(gofmt -l ./cmd ./internal)"
go build -o bin/mcpx ./cmd/mcpx-server
```

The `v0.1.0` release is built from the verified `main` commit. Future releases are created from `main` after the pull request and CI checks have passed.

## Learning and research disclaimer

MCPX is provided for learning, research, and authorized development-environment automation only. Users are responsible for deployment, configuration, command execution, file changes, credential handling, and any direct or indirect consequences. Do not use MCPX against systems, data, or networks without authorization. Before production use, perform a security review, back up relevant data, apply least-privilege policies, and verify human approval flows.

This project and its documentation are not security, legal, medical, financial, or other professional advice, and they are not guaranteed to fit any particular use case. Confirm the authorization scope and review commands and changes before operating on a real environment.

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=opentokenz/mcpx&type=Date)](https://www.star-history.com/#opentokenz/mcpx&Date)

## Acknowledgements

Thanks to the [LINUX DO](https://linux.do) community: **Learn AI, join LINUX DO.**

## License

This project is licensed under the [Apache License 2.0](LICENSE).
