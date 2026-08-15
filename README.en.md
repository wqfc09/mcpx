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

The Runtime does not cache the Workspace registry: listing, name resolution, and new Session creation reload the current Instance config. `mcpx workspace ...` first resolves the user-level Instance rendezvous and edits that Instance's Home, even when the calling shell has a different `MCPX_HOME`. Invalid/unverifiable Instance state is an explicit error rather than a silent fallback to another config. Existing Remote Sessions keep their stored Workspace path after registry rename/unregister, while new Sessions require a currently registered `ok` Workspace. Valid Workspace paths are canonicalized physically and hashed into a stable 16-hex Workspace ID, so logical rename does not change runtime identity.

### Instruction context

MCPX has one global natural-language instruction source: `~/.mcpx/system_prompt.md`. Global `AGENTS.md` discovery and the configurable `global_agents_path` are not used. Repository instructions live in the Workspace root and directory-level `AGENTS.md` files.

For a Workspace path, MCPX resolves one live instruction context in this order: global `system_prompt.md`, trusted MCP `initialize.instructions`, Workspace-root `AGENTS.md`, then narrower directory `AGENTS.md` files. Global `system_prompt.md` and Workspace `AGENTS.md` share the same instruction semantics inside the Runtime; only their discovery scope and priority differ.

Each `system_prompt.md` or `AGENTS.md` is limited to 64 KiB, and the default inline instruction-context budget is 256 KiB. SHA values are consistency/revision metadata, not trust approvals. Instruction content is live rather than frozen into a Remote Session. Use `runtime_read(view="instructions")` with optional `id`, `anchor_path`, or `paths` to read or resolve current instructions.

### Upstream MCP configuration

MCPX accepts exactly two MCP configuration sources: Global `~/.mcpx/.mcp.json` and Workspace `<workspace>/.mcpx/.mcp.json`. Global is the definition/authority layer. A Workspace may define a new ordinary MCP under a new name; when a name already exists Global, the Workspace entry is **activation-only** and may contain only `enabled`. It cannot redefine `command`, `args`, `env`, `trust`, `injectInstructions`, or Plugin identity.

Ordinary MCP registrations and Plugin activation support `enabled`, defaulting to `true`. A disabled ordinary MCP remains visible for inventory/debugging but is not callable. A Global Plugin definition always contributes stable process-wide `plugin.*` schemas through a one-shot catalog probe; Workspace `enabled` only controls whether the business runtime is active/callable and does not change `tools/list`. Changes to Global Plugin tools/schema definitions still require an Instance restart to rebuild the catalog.

Global `trust: true` is immediately effective. `trust: true` on a **new ordinary Workspace MCP** is a persistent trust request: the first actual call requires user confirmation, then MCPX stores the approval in `~/.mcpx/mcp-trust.json`. The approval is bound to the canonical Workspace path, registration name, and an internal registration fingerprint. Same-name Global activation entries cannot declare their own trust.

The current fingerprint covers `type`, `command`, `args`, `injectInstructions`, and the Plugin contract. Changes to those fields require reapproval. `enabled`, `trust`, `description`, and `env` do not currently affect the fingerprint; environment values are intentionally outside this trust check for now.

An MCP registration may set `injectInstructions: true` to expose `instructions` returned by the MCP initialize handshake, but automatic inclusion still requires effective trust. A Workspace may therefore request both `trust: true` and `injectInstructions: true`; its instructions remain excluded until trust is approved. Natural-language instruction content itself is not separately fingerprinted or Prompt-approved.

### Plugins

Plugin identity/definition remains Global-only. A Workspace may only activate/deactivate a same-name Plugin with `enabled`. Plugin V1 supports `runtime=mcp|controller`: MCP runtimes use a short-lived `MCPX_PLUGIN_CATALOG=1` probe at Instance startup to validate public tools/private Inbox and mount stable `plugin.<registration>.<tool>` schemas; Controller runtimes are MCPX-managed Workspace-local sidecars, do not participate in the MCP catalog, and expose no `plugin.<Controller>.*` tool surface.

```json
{
  "mcpServers": {
    "comet": {
      "type": "stdio",
      "command": "comet-mcp",
      "isPlugin": true,
      "trust": true,
      "plugin": {
        "runtime": "mcp",
        "scope": "workspace",
        "tools": ["context", "action", "doctor"],
        "inbox": "inbox"
      }
    }
  }
}
```

For MCP runtimes, omit `plugin.scope` or use `instance` to reuse one persistent MCP process across the Instance; use `workspace` for one lease per canonical Workspace ID. Controller V1 is always Workspace-scoped and MCPX owns its local sidecar lifecycle and durable Inbox.

Workspace-scoped Plugins run with the Workspace root as cwd; instance-scoped MCP Plugins run from the Instance Home. MCPX injects `MCPX_INSTANCE_ID`, `MCPX_INSTANCE_HOME`, `MCPX_PLUGIN_NAME`, `MCPX_PLUGIN_SCOPE`, and `MCPX_PLUGIN_RUNTIME_DIR`; Workspace scope additionally receives `MCPX_WORKSPACE`, `MCPX_WORKSPACE_ID`, and `MCPX_WORKSPACE_NAME`.

MCP runtimes declare explicit public `plugin.tools` and a private upstream `plugin.inbox`. Controller runtimes declare no MCP tools/inbox; instead they coordinate existing Plugins through Global `depends`, guarded `mounts`, Inbox `subscriptions`, and short Skill `contributes`, while MCPX hosts their durable Inbox. When the main model must cross a coordination hard gate it uses `plugin_tool(action="signal")`; MCPX verifies the current Remote Session/Workspace before delivering the owner signal.

The MCP Plugin catalog remains a process-wide schema snapshot created at Instance startup. Runtime calls re-check upstream schemas, and Controller automatic mounts additionally enforce Global string-argument guards in the Host. `trust: true` does not bypass schema checks, mount guards, upstream permissions, or upstream safety controls. See `docs/HANDOFF_PLUGIN_CONTROLLER.md` for the Controller V1 contract.

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

`tools/list` is the authoritative source for tool names, descriptions, input schemas, and annotations. The static public surface contains 20 tools: 13 core tools and 7 support tools. Configured Plugins can add dynamic `plugin.<registration>.<tool>` entries to that catalog.

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
