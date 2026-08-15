package workspace

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

var (
	ErrNotFound    = errors.New("workspace not found")
	ErrUnavailable = errors.New("workspace unavailable")
	ErrConflict    = errors.New("workspace conflict")
)

// Registry is a thin durable-config view. It intentionally keeps no Workspace
// cache: every read reloads global config so CLI changes are visible without a
// Runtime restart.
type Registry struct {
	globalPath string
}

// NewRegistry binds a registry to global config and validates the current
// durable registrations once. Subsequent reads always reload the file.
func NewRegistry(globalPath string) (*Registry, error) {
	if strings.TrimSpace(globalPath) == "" {
		var err error
		globalPath, err = config.GlobalConfigPath()
		if err != nil {
			return nil, err
		}
	}
	r := &Registry{globalPath: globalPath}
	if _, err := r.ListChecked(); err != nil {
		return nil, err
	}
	return r, nil
}

// List returns the current durable registrations in config order. It is the
// lightweight inventory view; strict callers should use ListChecked.
func (r *Registry) List() []Workspace {
	workspaces, _ := r.ListChecked()
	return workspaces
}

// ListChecked returns the current durable registrations and propagates config
// read errors for callers that must fail closed.
func (r *Registry) ListChecked() ([]Workspace, error) {
	_, workspaces, err := r.load()
	return workspaces, err
}

// Get returns the current durable registration by logical name. It is a
// lightweight lookup; strict callers should use Lookup or Resolve.
func (r *Registry) Get(name string) (Workspace, bool) {
	ws, ok, _ := r.Lookup(name)
	return ws, ok
}

// Lookup returns the current durable registration by logical name and exposes
// config read errors.
func (r *Registry) Lookup(name string) (Workspace, bool, error) {
	_, workspaces, err := r.load()
	if err != nil {
		return Workspace{}, false, err
	}
	name = strings.TrimSpace(name)
	for _, ws := range workspaces {
		if ws.Name == name {
			return ws, true, nil
		}
	}
	return Workspace{}, false, nil
}

// Resolve returns a registered Workspace only when its current path is usable.
func (r *Registry) Resolve(name string) (Workspace, error) {
	ws, ok, err := r.Lookup(name)
	if err != nil {
		return Workspace{}, err
	}
	if !ok {
		return Workspace{}, fmt.Errorf("%w: %q", ErrNotFound, strings.TrimSpace(name))
	}
	if ws.Status != StatusOK {
		return Workspace{}, fmt.Errorf("%w: %q path %s is %s", ErrUnavailable, ws.Name, ws.Path, ws.Status)
	}
	return ws, nil
}

// Register creates or updates one logical registration. When name is empty the
// Workspace directory basename is used. The target path must currently exist
// and be a directory; later path disappearance is represented as stale state.
func (r *Registry) Register(name, rawPath string) (Workspace, error) {
	absPath, err := normalizeWorkspacePath(rawPath)
	if err != nil {
		return Workspace{}, err
	}
	if status := workspacePathStatus(absPath); status != StatusOK {
		return Workspace{}, fmt.Errorf("%w: path %s is %s", ErrUnavailable, absPath, status)
	}
	absPath = canonicalWorkspacePath(absPath)
	name = strings.TrimSpace(name)
	if name == "" {
		name = filepath.Base(absPath)
	}
	if err := validateWorkspaceName(name); err != nil {
		return Workspace{}, err
	}

	cfg, current, err := r.load()
	if err != nil {
		return Workspace{}, err
	}
	index := -1
	for i, ws := range current {
		if ws.Path == absPath && ws.Name != name {
			return Workspace{}, fmt.Errorf("%w: path %s is already registered as %q", ErrConflict, absPath, ws.Name)
		}
		if ws.Name == name {
			index = i
		}
	}
	if index >= 0 {
		cfg.Workspaces[index].Name = name
		cfg.Workspaces[index].Path = absPath
	} else {
		cfg.Workspaces = append(cfg.Workspaces, config.WorkspaceEntry{Name: name, Path: absPath})
	}
	if err := config.WriteGlobal(r.globalPath, cfg); err != nil {
		return Workspace{}, err
	}
	ws, ok, err := r.Lookup(name)
	if err != nil {
		return Workspace{}, err
	}
	if !ok {
		return Workspace{}, fmt.Errorf("registered workspace %q disappeared after write", name)
	}
	return ws, nil
}

// Rename changes only the logical registry name and never moves filesystem data.
func (r *Registry) Rename(oldName, newName string) (Workspace, error) {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if err := validateWorkspaceName(newName); err != nil {
		return Workspace{}, err
	}
	cfg, current, err := r.load()
	if err != nil {
		return Workspace{}, err
	}
	oldIndex := -1
	for i, ws := range current {
		if ws.Name == newName && newName != oldName {
			return Workspace{}, fmt.Errorf("%w: workspace name %q is already registered", ErrConflict, newName)
		}
		if ws.Name == oldName {
			oldIndex = i
		}
	}
	if oldIndex < 0 {
		return Workspace{}, fmt.Errorf("%w: %q", ErrNotFound, oldName)
	}
	cfg.Workspaces[oldIndex].Name = newName
	if err := config.WriteGlobal(r.globalPath, cfg); err != nil {
		return Workspace{}, err
	}
	ws, ok, err := r.Lookup(newName)
	if err != nil {
		return Workspace{}, err
	}
	if !ok {
		return Workspace{}, fmt.Errorf("renamed workspace %q disappeared after write", newName)
	}
	return ws, nil
}

// Unregister removes only the durable registry entry. It never removes or
// modifies files under the Workspace path.
func (r *Registry) Unregister(name string) (Workspace, error) {
	name = strings.TrimSpace(name)
	cfg, current, err := r.load()
	if err != nil {
		return Workspace{}, err
	}
	index := -1
	var removed Workspace
	for i, ws := range current {
		if ws.Name == name {
			index, removed = i, ws
			break
		}
	}
	if index < 0 {
		return Workspace{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	cfg.Workspaces = append(cfg.Workspaces[:index], cfg.Workspaces[index+1:]...)
	if err := config.WriteGlobal(r.globalPath, cfg); err != nil {
		return Workspace{}, err
	}
	return removed, nil
}

// Prune returns stale registrations. With apply=false it is a dry run; with
// apply=true only missing/invalid registry entries are removed from config.
func (r *Registry) Prune(apply bool) ([]Workspace, error) {
	cfg, current, err := r.load()
	if err != nil {
		return nil, err
	}
	stale := make([]Workspace, 0)
	staleNames := map[string]bool{}
	for _, ws := range current {
		if ws.Status == StatusOK {
			continue
		}
		stale = append(stale, ws)
		staleNames[ws.Name] = true
	}
	if !apply || len(stale) == 0 {
		return stale, nil
	}
	kept := make([]config.WorkspaceEntry, 0, len(cfg.Workspaces)-len(stale))
	for _, entry := range cfg.Workspaces {
		if !staleNames[strings.TrimSpace(entry.Name)] {
			kept = append(kept, entry)
		}
	}
	cfg.Workspaces = kept
	if err := config.WriteGlobal(r.globalPath, cfg); err != nil {
		return nil, err
	}
	return stale, nil
}

func (r *Registry) load() (config.Config, []Workspace, error) {
	cfg, err := config.LoadGlobal(r.globalPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	workspaces := make([]Workspace, 0, len(cfg.Workspaces))
	seenNames := map[string]bool{}
	seenPaths := map[string]string{}
	for _, entry := range cfg.Workspaces {
		name := strings.TrimSpace(entry.Name)
		if err := validateWorkspaceName(name); err != nil {
			return config.Config{}, nil, fmt.Errorf("invalid workspace registration: %w", err)
		}
		absPath, err := normalizeWorkspacePath(entry.Path)
		if err != nil {
			return config.Config{}, nil, fmt.Errorf("workspace %q: %w", name, err)
		}
		status := workspacePathStatus(absPath)
		if status == StatusOK {
			absPath = canonicalWorkspacePath(absPath)
		}
		if seenNames[name] {
			return config.Config{}, nil, fmt.Errorf("%w: duplicate workspace name %q", ErrConflict, name)
		}
		if owner, ok := seenPaths[absPath]; ok {
			return config.Config{}, nil, fmt.Errorf("%w: workspace path %s is registered by both %q and %q", ErrConflict, absPath, owner, name)
		}
		seenNames[name] = true
		seenPaths[absPath] = name
		workspaces = append(workspaces, Workspace{
			ID: IDForPath(absPath), Name: name, Path: absPath, Description: entry.Description,
			Status: status,
		})
	}
	return cfg, workspaces, nil
}

// IDForPath returns a stable runtime identity for a Workspace path. It is
// independent of the logical registry name so rename does not move Plugin
// runtime leases. The 16-hex format intentionally matches the launcher-era ID.
func IDForPath(path string) string {
	clean := filepath.Clean(strings.TrimSpace(path))
	digest := sha256.Sum256([]byte(clean))
	return hex.EncodeToString(digest[:8])
}

func normalizeWorkspacePath(rawPath string) (string, error) {
	path := strings.TrimSpace(config.ExpandHome(rawPath))
	if path == "" {
		return "", fmt.Errorf("workspace path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	return filepath.Clean(absPath), nil
}

func canonicalWorkspacePath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(resolved)
}

func workspacePathStatus(path string) string {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return StatusOK
		}
		return StatusInvalid
	}
	if errors.Is(err, os.ErrNotExist) {
		return StatusMissing
	}
	return StatusInvalid
}

func validateWorkspaceName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("workspace name is required")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return fmt.Errorf("invalid workspace name %q", name)
	}
	return nil
}
