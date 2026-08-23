// Package instruction discovers the natural-language instruction documents
// intentionally exposed to Remote Session clients.
package instruction

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcpx/internal/config"
)

const (
	// MaxDocumentBytes bounds every global system_prompt.md or AGENTS.md source.
	MaxDocumentBytes int64 = 64 << 10
	// MaxContextBytes is the default inline budget for one resolved instruction chain.
	MaxContextBytes int64 = 256 << 10
)

var ErrNotFound = errors.New("agent instruction not found")

// Document is a machine-readable instruction descriptor. Absolute host paths
// are deliberately kept private; callers address a document through its ID.
type Document struct {
	ID          string `json:"id"`
	Scope       string `json:"scope"` // global | project | directory
	Name        string `json:"name"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	RelativeDir string `json:"relative_dir,omitempty"`
	AppliesTo   string `json:"applies_to,omitempty"`
	Priority    int    `json:"priority"`
	Active      bool   `json:"active"`
	Reason      string `json:"reason,omitempty"`
	path        string
}

// Discover returns ~/.mcpx/system_prompt.md followed by the Workspace root
// AGENTS.md. Directory AGENTS.md files are resolved only when an anchor is used.
func Discover(workspaceRoot string) []Document {
	return DiscoverAt(workspaceRoot, "")
}

// DiscoverAt discovers instruction documents for an optional workspace-relative
// anchor. Order is broad to narrow: global system_prompt.md, project AGENTS.md,
// then each directory AGENTS.md from the Workspace root down to the anchor.
func DiscoverAt(workspaceRoot, anchorPath string) []Document {
	documents := make([]Document, 0, 8)
	if globalPath, err := config.GlobalSystemPromptPath(); err == nil {
		if document, ok := inspect("global", "global", "", "*", globalPath, 10); ok {
			document.Active = true
			document.Reason = "global_system_prompt"
			documents = append(documents, document)
		}
	}
	if workspaceRoot == "" {
		return markActiveChain(documents)
	}
	if document, ok := inspect("project", "project", ".", "**", filepath.Join(workspaceRoot, "AGENTS.md"), 30); ok {
		document.Active = true
		document.Reason = "workspace_root"
		documents = append(documents, document)
	}
	anchor := filepath.ToSlash(filepath.Clean(strings.TrimSpace(anchorPath)))
	if anchor == "." || anchor == "" {
		return markActiveChain(documents)
	}
	absoluteCandidate := filepath.Join(workspaceRoot, filepath.FromSlash(anchor))
	info, err := os.Stat(absoluteCandidate)
	dirRel := anchor
	if err == nil && !info.IsDir() {
		dirRel = filepath.ToSlash(filepath.Dir(anchor))
	}
	if dirRel == "." {
		return markActiveChain(documents)
	}
	parts := strings.Split(dirRel, "/")
	accum := make([]string, 0, len(parts))
	priority := 40
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		accum = append(accum, part)
		relDir := strings.Join(accum, "/")
		id := "dir:" + relDir
		path := filepath.Join(workspaceRoot, filepath.FromSlash(relDir), "AGENTS.md")
		if document, ok := inspect(id, "directory", relDir, relDir+"/**", path, priority); ok {
			document.Active = true
			document.Reason = "directory_chain"
			documents = append(documents, document)
			priority += 10
		}
	}
	return markActiveChain(documents)
}

// ResolveForPaths returns per-path instruction chains and cross-path conflicts.
func ResolveForPaths(workspaceRoot string, paths []string) map[string]any {
	byPath := make(map[string]any, len(paths))
	conflicts := []map[string]any{}
	for _, path := range paths {
		byPath[path] = DiscoverAt(workspaceRoot, path)
	}
	frontends, backends := false, false
	for path := range byPath {
		if strings.HasPrefix(filepath.ToSlash(path), "frontend/") {
			frontends = true
		}
		if strings.HasPrefix(filepath.ToSlash(path), "backend/") {
			backends = true
		}
	}
	if frontends && backends {
		conflicts = append(conflicts, map[string]any{
			"code":    "cross_tree_rules",
			"message": "operation spans frontend and backend instruction trees; resolve per file",
			"paths":   paths,
		})
	}
	return map[string]any{"by_path": byPath, "conflicts": conflicts}
}

// Read resolves a previously discoverable document by ID.
func Read(workspaceRoot, id string) (Document, string, error) {
	return ReadAt(workspaceRoot, "", id)
}

// ReadAt discovers with anchor then reads by id.
func ReadAt(workspaceRoot, anchorPath, id string) (Document, string, error) {
	for _, document := range DiscoverAt(workspaceRoot, anchorPath) {
		if document.ID == id {
			return readDocument(document)
		}
	}
	if strings.HasPrefix(id, "dir:") && workspaceRoot != "" {
		rel := strings.TrimPrefix(id, "dir:")
		path := filepath.Join(workspaceRoot, filepath.FromSlash(rel), "AGENTS.md")
		if document, ok := inspect(id, "directory", rel, rel+"/**", path, 0); ok {
			return readDocument(document)
		}
	}
	if id == "global" || id == "project" {
		for _, document := range Discover(workspaceRoot) {
			if document.ID == id {
				return readDocument(document)
			}
		}
	}
	return Document{}, "", ErrNotFound
}

// ReadContents loads instruction text until totalBudget is exhausted. The SHA
// in each descriptor is used only as a read-consistency guard, not as trust.
func ReadContents(documents []Document, totalBudget int64) ([]map[string]any, int64) {
	if totalBudget <= 0 {
		totalBudget = MaxContextBytes
	}
	out := make([]map[string]any, 0, len(documents))
	var used int64
	for _, document := range documents {
		item := documentMap(document)
		if used >= totalBudget || document.Bytes > totalBudget-used {
			item["content_omitted"] = true
			item["reason_omitted"] = "budget_exceeded"
			out = append(out, item)
			continue
		}
		_, content, err := readDocument(document)
		if err != nil {
			item["content_omitted"] = true
			item["reason_omitted"] = err.Error()
			out = append(out, item)
			continue
		}
		item["content"] = content
		used += int64(len(content))
		out = append(out, item)
	}
	return out, used
}

func documentMap(document Document) map[string]any {
	return map[string]any{
		"id": document.ID, "scope": document.Scope, "name": document.Name,
		"sha256": document.SHA256, "bytes": document.Bytes, "priority": document.Priority,
		"relative_dir": document.RelativeDir, "applies_to": document.AppliesTo,
		"active": document.Active, "reason": document.Reason,
	}
}

func readDocument(document Document) (Document, string, error) {
	content, err := os.ReadFile(document.path)
	if err != nil {
		return Document{}, "", fmt.Errorf("read %s: %w", document.Name, err)
	}
	if int64(len(content)) != document.Bytes {
		return Document{}, "", fmt.Errorf("%s changed during read", document.Name)
	}
	digest := sha256.Sum256(content)
	if "sha256:"+hex.EncodeToString(digest[:]) != document.SHA256 {
		return Document{}, "", fmt.Errorf("%s changed during read", document.Name)
	}
	return document, string(content), nil
}

func inspect(id, scope, relativeDir, appliesTo, path string, priority int) (Document, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > MaxDocumentBytes {
		return Document{}, false
	}
	content, err := os.ReadFile(path)
	if err != nil || int64(len(content)) != info.Size() {
		return Document{}, false
	}
	digest := sha256.Sum256(content)
	return Document{
		ID: id, Scope: scope, Name: filepath.Base(path), Bytes: info.Size(),
		SHA256: "sha256:" + hex.EncodeToString(digest[:]), path: path,
		RelativeDir: relativeDir, AppliesTo: appliesTo, Priority: priority,
	}, true
}

func markActiveChain(documents []Document) []Document {
	for i := range documents {
		if documents[i].Priority == 0 {
			documents[i].Priority = (i + 1) * 10
		}
		if !documents[i].Active {
			documents[i].Active = true
		}
	}
	return documents
}
