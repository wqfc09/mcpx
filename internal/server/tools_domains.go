package server

// Domain ownership for the public MCP tool surface (file-cluster layout).
// Full subpackages under tools/<domain> are deferred: handlers remain *Runtime
// methods so they can share envelope/auth/session helpers without export churn.
//
// Domain              | Public tools                         | Primary files
// --------------------|--------------------------------------|---------------------------
// session             | session                              | tools_public_adapters.go, tools_session_open.go, tools_remote_session.go
// source              | read                                 | tools_read.go, tools_source_unified.go
// edit                | edit, move_out                       | tools_edit.go, tools_workspace_delete.go
// command / task      | execute, observe                     | tools_execute.go, tools_command_execute.go, tools_observe.go
// plan                | plan                                 | tools_plan_clean.go, tools_plan.go
// operation           | operation_batch, operation_manage    | tools_operation.go, operation_runtime.go
// runtime / env       | runtime_read, environment_read, environment | tools_environment.go, tools_instruction.go
// extension           | skill_tool, mcp_tool, plugin_tool    | tools_discover.go, tools_ext.go, tools_plugin.go
// artifact            | artifact                             | tools_artifact.go (+ internal/artifact)
// screenshot / secret | screenshot_capture, secret_provide   | tools_screenshot.go, tools_manage / secrets
// catalog / prompts   | tools/list registration              | tools_catalog.go, prompts/, guidance/
//
// Public dispatch entry points are registered in tools_clean_core.go.
