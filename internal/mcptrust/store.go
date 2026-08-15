package mcptrust

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Approval struct {
	Workspace    string    `json:"workspace"`
	Registration string    `json:"registration"`
	Fingerprint  string    `json:"fingerprint"`
	ApprovedAt   time.Time `json:"approved_at"`
}

type fileState struct {
	Approvals []Approval `json:"approvals"`
}

// Store persists machine-level approvals for Workspace MCP trust requests.
type Store struct {
	mu   sync.Mutex
	path string
}

func Open(path string) (*Store, error) {
	store := &Store{path: path}
	store.mu.Lock()
	defer store.mu.Unlock()
	_, err := store.loadLocked()
	if err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) IsApproved(workspace, registration, fingerprint string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	workspace = strings.TrimSpace(workspace)
	registration = strings.TrimSpace(registration)
	fingerprint = strings.TrimSpace(fingerprint)
	if workspace == "" || registration == "" || fingerprint == "" {
		return false, nil
	}
	workspace = canonicalWorkspacePath(workspace)
	for _, approval := range state.Approvals {
		if approval.Workspace == workspace && approval.Registration == registration && approval.Fingerprint == fingerprint {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) Approve(workspace, registration, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return err
	}
	workspace = strings.TrimSpace(workspace)
	registration = strings.TrimSpace(registration)
	fingerprint = strings.TrimSpace(fingerprint)
	if workspace == "" || registration == "" || fingerprint == "" {
		return fmt.Errorf("workspace, registration, and fingerprint are required")
	}
	workspace = canonicalWorkspacePath(workspace)
	updated := false
	for i := range state.Approvals {
		if state.Approvals[i].Workspace == workspace && state.Approvals[i].Registration == registration {
			state.Approvals[i] = Approval{Workspace: workspace, Registration: registration, Fingerprint: fingerprint, ApprovedAt: time.Now().UTC()}
			updated = true
			break
		}
	}
	if !updated {
		state.Approvals = append(state.Approvals, Approval{Workspace: workspace, Registration: registration, Fingerprint: fingerprint, ApprovedAt: time.Now().UTC()})
	}
	return s.writeLocked(state)
}

func canonicalWorkspacePath(path string) string {
	clean := filepath.Clean(strings.TrimSpace(path))
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return clean
	}
	return filepath.Clean(resolved)
}

func (s *Store) loadLocked() (fileState, error) {
	state := fileState{Approvals: []Approval{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fileState{}, fmt.Errorf("parse MCP trust store %s: %w", s.path, err)
	}
	if state.Approvals == nil {
		state.Approvals = []Approval{}
	}
	return state, nil
}

func (s *Store) writeLocked(state fileState) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(s.path, 0o600)
}
