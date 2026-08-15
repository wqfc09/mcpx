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
		if name != "" && name != session.WorkspaceName {
			return workspace.Workspace{}, remoteID, fmt.Errorf("%w: workspace does not match Remote Session", remotesession.ErrInvalidInput)
		}
		// A Remote Session owns its frozen Workspace path. Registry rename,
		// unregister, or path staleness must not invalidate an existing session.
		registered, ok := r.reg.Get(session.WorkspaceName)
		if ok {
			registered.Path = session.WorkspacePath
			registered.Status = workspace.StatusOK
			return registered, remoteID, nil
		}
		return workspace.Workspace{
			ID: session.WorkspaceName, Name: session.WorkspaceName, Path: session.WorkspacePath, Status: workspace.StatusOK,
		}, remoteID, nil
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
