package pluginstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const Version = 1

type Status struct {
	Version         int      `json:"version"`
	Plugin          string   `json:"plugin"`
	Runtime         string   `json:"runtime"`
	Scope           string   `json:"scope"`
	WorkspaceID     string   `json:"workspace_id,omitempty"`
	WorkspaceName   string   `json:"workspace_name,omitempty"`
	PID             int      `json:"pid"`
	Executable      string   `json:"executable"`
	Argv0           string   `json:"argv0,omitempty"`
	Args            []string `json:"args"`
	StartedAt       string   `json:"started_at"`
	RuntimeRevision string   `json:"runtime_revision"`
	Generation      string   `json:"generation,omitempty"`
}

func Path(runtimeDir string) string { return filepath.Join(runtimeDir, "lease.json") }

func Write(runtimeDir string, status Status) error {
	status.Version = Version
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmp, err := os.CreateTemp(runtimeDir, ".lease-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, Path(runtimeDir))
}

func Read(runtimeDir string) (Status, error) {
	data, err := os.ReadFile(Path(runtimeDir))
	if err != nil {
		return Status{}, err
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		return Status{}, err
	}
	if status.Version != Version || strings.TrimSpace(status.Plugin) == "" || status.PID <= 0 || strings.TrimSpace(status.Executable) == "" {
		return Status{}, errors.New("invalid Plugin lease status")
	}
	return status, nil
}

func Remove(runtimeDir string) error {
	if strings.TrimSpace(runtimeDir) == "" {
		return nil
	}
	if err := os.Remove(Path(runtimeDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
