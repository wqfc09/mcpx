package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/audit"
	"mcpx/internal/observation"
	"mcpx/internal/projecttask"
	"mcpx/internal/remotesession"
	"mcpx/internal/skill"
	buildversion "mcpx/internal/version"
)

// toolSessionOpen creates or reuses a Remote Session and returns a full bootstrap bundle
// so clients need only one MCP call to start developing.
func (r *Runtime) toolSessionOpen(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}

	includeInstrContent := false
	if v, ok := envReq.Payload["include_instructions_content"].(bool); ok {
		includeInstrContent = v
	}
	includeProjectTasks := false
	if v, ok := envReq.Payload["include_project_tasks"].(bool); ok {
		includeProjectTasks = v
	}
	var session remotesession.Session
	remoteID, _ := envReq.Payload["remote_session_id"].(string)
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		remoteID = strings.TrimSpace(envReq.RemoteSessionID)
	}

	workspaceName := strings.TrimSpace(envReq.Workspace)
	if workspaceName == "" {
		workspaceName, _ = envReq.Payload["workspace"].(string)
	}
	if remoteID != "" {
		existing, err := r.remote.Get(ctx, principal, remoteID)
		if err != nil {
			return r.remoteError(envReq, remoteID, workspaceName, err)
		}
		session = existing
		workspaceName = session.WorkspaceName
	} else {
		created, err := r.createRemoteSession(ctx, principal, envReq, workspaceName)
		if err != nil {
			return r.remoteError(envReq, "", workspaceName, err)
		}
		session = created.Session
	}

	wsPath := session.WorkspacePath
	effective := r.effectiveConfig(wsPath)
	tools := r.runtimeToolCapabilities(effective, &session)

	var (
		servers              = []map[string]any{}
		skills               = []map[string]any{}
		project              map[string]any
		gitHead              string
		treeDigest           string
		pendingConfirmations []map[string]any
		taskList             any
		artifacts            any
		latestModelState     any
	)
	var tasks any
	var bootstrap sync.WaitGroup
	bootstrap.Add(6)
	go func() {
		defer bootstrap.Done()
		if manager, err := r.mcpManagerForWorkspace(wsPath); err == nil && effective.Discovery.MCP.Enabled {
			servers = manager.List()
		}
	}()
	go func() {
		defer bootstrap.Done()
		if effective.Discovery.Skills.Enabled {
			skills = skillItems(skill.LoadAll(effective.Discovery.Skills.Dirs, wsPath))
		}
	}()
	go func() {
		defer bootstrap.Done()
		project = inspectProject(ctx, wsPath)
		if includeProjectTasks {
			tasks = projecttask.Discover(wsPath)
		}
	}()
	go func() {
		defer bootstrap.Done()
		gitHead, treeDigest = workspaceRevision(ctx, wsPath)
	}()
	go func() {
		defer bootstrap.Done()
		pendingConfirmations = pendingConfirmationItems(r.approvals.ListRemoteSession(session.ID))
		taskList, _ = r.tasks.List(session.ID, 20)
		artifacts, _ = r.artifacts.List(ctx, session.ID, "", 20)
	}()
	go func() {
		defer bootstrap.Done()
		if r.observation == nil || r.observation.store == nil {
			return
		}
		page, err := r.observation.store.QueryMemory(ctx, observation.MemoryQuery{
			Workspace: session.WorkspaceName,
			SessionID: session.ID,
			Type:      "progress",
			Latest:    1,
		})
		if err == nil && len(page.Items) > 0 {
			latestModelState = page.Items[0]
		}
	}()
	bootstrap.Wait()
	servers = removePluginServerItems(servers)
	plugins := r.pluginInventory()

	instructionPayload := r.instructionContext(ctx, wsPath, "", includeInstrContent)
	instructionDocuments, _ := instructionPayload["documents"].([]map[string]any)
	toolManifest := r.registeredToolManifest()
	build := r.build
	if build.Version == "" {
		build.Version = buildversion.Current
	}

	guidance := agentGuidance()
	clientProtocol := clientProtocolCapabilities()
	revisions := map[string]any{
		"tool_schema_revision":         r.currentToolSchemaRevision(),
		"capability_manifest_revision": capabilityManifestRevision(toolManifest, skills, map[string]any{"mcp_servers": servers, "plugins": plugins}, instructionDocuments, guidance, clientProtocol),
		"guidance_revision":            agentGuidanceRevision(),
		"instruction_revision":         instructionRevision(instructionDocuments),
		"session_capability_revision":  sessionCapabilityRevision(&session),
		"client_protocol_revision":     clientProtocolRevision(),
	}

	data := map[string]any{
		"remote_session_id": session.ID,
		"mcpx": map[string]any{
			"version": build.Version, "commit": build.Commit, "build_time": build.Date,
		},
		"remote_session": map[string]any{
			"id": session.ID, "role": session.Role, "status": session.Status,
			"version": session.Version, "label": session.Label, "description": session.Description,
			"workspace_name": session.WorkspaceName, "workspace_path": session.WorkspacePath,
		},
		"workspace": map[string]any{
			"name": session.WorkspaceName, "path": session.WorkspacePath,
			"git_head": gitHead, "tree_digest": treeDigest,
		},
		"revisions":       revisions,
		"agent_guidance":  guidance,
		"client_protocol": clientProtocol,
		"tools":           tools,
		"extension_inventory": map[string]any{
			"skills":      compactSkillMaps(skills),
			"mcp_servers": compactMCPServerInventory(servers),
			"plugins":     plugins,
		},
		"instructions":  instructionPayload,
		"project":       project,
		"project_tasks": tasks,
		"git": map[string]any{
			"head": gitHead, "tree_digest": treeDigest,
		},
		"pending_confirmations": pendingConfirmations,
		"tasks":                 taskList,
		"artifacts":             artifacts,
		"schema_source":         "tools/list",
		"capability_version":    cleanCoreCapabilityVersion,
		"capability_groups":     capabilityGroups(),
		"recommended_workflows": map[string]any{
			"bootstrap":      []string{"workspace", "session"},
			"source_change":  []string{"read", "edit", "execute", "observe"},
			"plan_delivery":  []string{"plan", "edit", "execute", "artifact", "observe"},
			"extension_call": []string{"skill_tool", "mcp_tool", "plugin_tool", "plugin.<registration>.<tool>"},
		},
		"opened_at": time.Now().UTC().Format(time.RFC3339),
	}
	if latestModelState != nil {
		data["latest_model_state"] = latestModelState
	}

	r.logAudit(audit.Event{
		RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName,
		Tool: "session", Status: "ok",
	})
	return compactToolResult(data, fmt.Sprintf("Session %s opened for workspace %s.", session.ID, session.WorkspaceName)), nil
}
