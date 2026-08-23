package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mcpx/internal/auth"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
	"mcpx/internal/workspace"
)

// resolveExplicitWorkspace resolves only caller-provided state. It never reads
// or mutates an MCP transport session.
func (r *Runtime) resolveExplicitWorkspace(ctx context.Context, principal auth.Principal, req envelope.Request) (workspace.Workspace, string, error) {
	name := strings.TrimSpace(req.Workspace)
	if name == "" {
		name, _ = req.Payload["workspace"].(string)
		name = strings.TrimSpace(name)
	}
	remoteID := remoteSessionID(req)
	if remoteID != "" {
		session, err := r.remote.Get(ctx, principal, remoteID)
		if err != nil {
			return workspace.Workspace{}, remoteID, err
		}
		registered, err := r.resolveSessionWorkspace(ctx, session)
		if err != nil {
			return workspace.Workspace{}, remoteID, err
		}
		if name != "" && name != registered.Name {
			return workspace.Workspace{}, remoteID, fmt.Errorf("%w: workspace does not match Remote Session", remotesession.ErrInvalidInput)
		}
		return registered, remoteID, nil
	}
	if name == "" {
		return workspace.Workspace{}, "", nil
	}
	registered, err := r.resolveRegisteredWorkspace(name)
	if err != nil {
		return workspace.Workspace{}, "", err
	}
	return registered, "", nil
}

func (r *Runtime) resolveSessionWorkspace(ctx context.Context, session remotesession.Session) (workspace.Workspace, error) {
	if id := strings.TrimSpace(session.WorkspaceID); id != "" {
		registered, err := r.reg.ResolveID(id)
		if err == nil {
			return registered, nil
		}
		switch {
		case errors.Is(err, workspace.ErrNotFound):
			return workspace.Workspace{}, fmt.Errorf("%w: workspace id %q", errWorkspaceUnregistered, id)
		case errors.Is(err, workspace.ErrUnavailable):
			return workspace.Workspace{}, fmt.Errorf("%w: %v", errWorkspaceUnavailable, err)
		default:
			return workspace.Workspace{}, err
		}
	}

	// Legacy rows created before workspace_id are reconciled once. The snapshot
	// path is used only to identify the current Registry entry; execution never
	// falls back to it.
	workspaces, err := r.reg.ListChecked()
	if err != nil {
		return workspace.Workspace{}, err
	}
	for _, candidate := range workspaces {
		if candidate.Name != session.WorkspaceName && candidate.Path != session.WorkspacePath {
			continue
		}
		if candidate.Status != workspace.StatusOK {
			return workspace.Workspace{}, fmt.Errorf("%w: %q path %s is %s", errWorkspaceUnavailable, candidate.Name, candidate.Path, candidate.Status)
		}
		if err := r.remote.BindWorkspaceID(ctx, session.ID, candidate.ID); err != nil {
			return workspace.Workspace{}, err
		}
		return candidate, nil
	}
	return workspace.Workspace{}, fmt.Errorf("%w: legacy session %s no longer matches a registered Workspace", errWorkspaceUnregistered, session.ID)
}

func (r *Runtime) resolveRegisteredWorkspace(name string) (workspace.Workspace, error) {
	registered, err := r.reg.Resolve(name)
	if err == nil {
		return registered, nil
	}
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return workspace.Workspace{}, fmt.Errorf("%w: %q", errWorkspaceNotFound, strings.TrimSpace(name))
	case errors.Is(err, workspace.ErrUnavailable):
		return workspace.Workspace{}, fmt.Errorf("%w: %v", errWorkspaceUnavailable, err)
	default:
		return workspace.Workspace{}, err
	}
}
