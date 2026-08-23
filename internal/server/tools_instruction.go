package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/envelope"
	"mcpx/internal/instruction"
	"mcpx/internal/mcpproxy"
)

const mcpInstructionPriority = 20

func (r *Runtime) toolAgentInstructionList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, remote, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	if id, _ := envReq.Payload["id"].(string); strings.TrimSpace(id) != "" {
		return r.toolAgentInstructionRead(ctx, req)
	}
	anchor, _ := envReq.Payload["anchor_path"].(string)
	contextData := r.instructionContext(ctx, remote.WorkspacePath, anchor, false)
	documents, _ := contextData["documents"].([]map[string]any)
	data := map[string]any{
		"instructions":         documents,
		"anchor_path":          anchor,
		"order":                []string{"global", "extension", "project", "directory"},
		"instruction_revision": instructionRevision(documents),
	}
	if failures := contextData["errors"]; failures != nil {
		data["errors"] = failures
	}
	var paths []string
	if raw, ok := envReq.Payload["paths"].([]any); ok {
		for _, item := range raw {
			if path, ok := item.(string); ok && strings.TrimSpace(path) != "" {
				paths = append(paths, path)
			}
		}
	}
	if len(paths) > 0 {
		data["resolution"] = r.instructionResolution(ctx, remote.WorkspacePath, paths)
	}
	return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, data)
}

func (r *Runtime) toolAgentInstructionRead(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, remote, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	id, _ := envReq.Payload["id"].(string)
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "mcp:") {
		document, content, err := r.readMCPInstruction(ctx, remote.WorkspacePath, strings.TrimPrefix(id, "mcp:"))
		if err != nil {
			code := "instruction_read_error"
			if errors.Is(err, instruction.ErrNotFound) {
				code = "instruction_not_found"
			}
			response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, nil, code, err.Error())
			response.RemoteSessionID = remote.ID
			return r.resultJSON(response)
		}
		return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, map[string]any{"instruction": document, "content": content})
	}
	anchor, _ := envReq.Payload["anchor_path"].(string)
	document, content, err := instruction.ReadAt(remote.WorkspacePath, anchor, id)
	if err != nil {
		code := "instruction_read_error"
		if errors.Is(err, instruction.ErrNotFound) {
			code = "instruction_not_found"
		}
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, nil, code, err.Error())
		response.RemoteSessionID = remote.ID
		return r.resultJSON(response)
	}
	return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, map[string]any{"instruction": document, "content": content})
}

func (r *Runtime) instructionContext(ctx context.Context, workspacePath, anchor string, inline bool) map[string]any {
	fileDocs := instruction.DiscoverAt(workspacePath, anchor)
	fileItems := fileInstructionItems(fileDocs, inline)
	extensionItems, failures := r.mcpInstructionItems(ctx, workspacePath, inline)
	items := mergeInstructionItems(fileItems, extensionItems)
	if inline {
		applyInstructionInlineBudget(items, instruction.MaxContextBytes)
	}
	data := map[string]any{
		"documents": items,
		"inline":    inline,
		"order":     []string{"global", "extension", "project", "directory"},
	}
	if len(failures) > 0 {
		data["errors"] = failures
	}
	return data
}

func (r *Runtime) instructionResolution(ctx context.Context, workspacePath string, paths []string) map[string]any {
	extensions, failures := r.mcpInstructionItems(ctx, workspacePath, false)
	byPath := make(map[string]any, len(paths))
	for _, path := range paths {
		files := fileInstructionItems(instruction.DiscoverAt(workspacePath, path), false)
		byPath[path] = mergeInstructionItems(files, extensions)
	}
	result := instruction.ResolveForPaths(workspacePath, paths)
	result["by_path"] = byPath
	if len(failures) > 0 {
		result["errors"] = failures
	}
	return result
}

func fileInstructionItems(documents []instruction.Document, inline bool) []map[string]any {
	if inline {
		budget := int64(len(documents)) * instruction.MaxDocumentBytes
		if budget <= 0 {
			budget = instruction.MaxContextBytes
		}
		items, _ := instruction.ReadContents(documents, budget)
		return items
	}
	items := make([]map[string]any, 0, len(documents))
	for _, document := range documents {
		items = append(items, map[string]any{
			"id": document.ID, "scope": document.Scope, "name": document.Name,
			"sha256": document.SHA256, "bytes": document.Bytes, "priority": document.Priority,
			"relative_dir": document.RelativeDir, "applies_to": document.AppliesTo,
			"active": document.Active, "reason": document.Reason,
		})
	}
	return items
}

func (r *Runtime) mcpInstructionItems(ctx context.Context, workspacePath string, inline bool) ([]map[string]any, []map[string]any) {
	if !r.effectiveConfig(workspacePath).Discovery.MCP.Enabled {
		return nil, nil
	}
	manager, err := r.mcpManagerForWorkspace(workspacePath)
	if err != nil {
		return nil, []map[string]any{{"scope": "extension", "code": "mcp_manager_unavailable", "message": err.Error()}}
	}
	servers := manager.Servers()
	names := make([]string, 0, len(servers))
	for name, server := range servers {
		if server.Trust && server.InjectInstructions {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	items := make([]map[string]any, 0, len(names))
	failures := make([]map[string]any, 0)
	for _, name := range names {
		document, content, err := readMCPInstructionContent(ctx, name, servers[name])
		if err != nil {
			failures = append(failures, map[string]any{"id": "mcp:" + name, "scope": "extension", "source": name, "code": "instruction_unavailable", "message": err.Error()})
			continue
		}
		if document == nil {
			continue
		}
		item := document
		if inline {
			item["content"] = content
		}
		items = append(items, item)
	}
	return items, failures
}

func (r *Runtime) readMCPInstruction(ctx context.Context, workspacePath, name string) (map[string]any, string, error) {
	if !r.effectiveConfig(workspacePath).Discovery.MCP.Enabled {
		return nil, "", instruction.ErrNotFound
	}
	manager, err := r.mcpManagerForWorkspace(workspacePath)
	if err != nil {
		return nil, "", err
	}
	server, ok := manager.ServerConfig(name)
	if !ok || !server.Trust || !server.InjectInstructions {
		return nil, "", instruction.ErrNotFound
	}
	document, content, err := readMCPInstructionContent(ctx, name, server)
	if err != nil {
		return nil, "", err
	}
	if document == nil {
		return nil, "", instruction.ErrNotFound
	}
	return document, content, nil
}

func readMCPInstructionContent(ctx context.Context, name string, server config.MCPServer) (map[string]any, string, error) {
	content, err := mcpproxy.InitializeInstructions(ctx, server)
	if err != nil {
		return nil, "", fmt.Errorf("read initialize.instructions from MCP server %q: %w", name, err)
	}
	if strings.TrimSpace(content) == "" {
		return nil, "", nil
	}
	if err := validateInstructionText(name, content); err != nil {
		return nil, "", err
	}
	return map[string]any{
		"id":       "mcp:" + name,
		"scope":    "extension",
		"name":     "initialize.instructions",
		"source":   name,
		"sha256":   instructionDigest(content),
		"bytes":    len(content),
		"priority": mcpInstructionPriority,
		"active":   true,
		"reason":   "trusted_mcp_instructions",
	}, content, nil
}

func mergeInstructionItems(groups ...[]map[string]any) []map[string]any {
	var items []map[string]any
	for _, group := range groups {
		items = append(items, group...)
	}
	sort.SliceStable(items, func(i, j int) bool {
		left, _ := items[i]["priority"].(int)
		right, _ := items[j]["priority"].(int)
		if left != right {
			return left < right
		}
		return fmt.Sprint(items[i]["id"]) < fmt.Sprint(items[j]["id"])
	})
	return items
}

func applyInstructionInlineBudget(items []map[string]any, budget int64) {
	if budget <= 0 {
		budget = instruction.MaxContextBytes
	}
	var used int64
	for _, item := range items {
		content, ok := item["content"].(string)
		if !ok {
			continue
		}
		size := int64(len(content))
		if size > budget-used {
			delete(item, "content")
			item["content_omitted"] = true
			item["reason_omitted"] = "budget_exceeded"
			continue
		}
		used += size
	}
}

func instructionDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validateInstructionText(source, content string) error {
	if !utf8.ValidString(content) {
		return fmt.Errorf("%s returned non-UTF-8 instructions", source)
	}
	if int64(len(content)) > instruction.MaxDocumentBytes {
		return fmt.Errorf("%s instructions exceed %d bytes", source, instruction.MaxDocumentBytes)
	}
	return nil
}
