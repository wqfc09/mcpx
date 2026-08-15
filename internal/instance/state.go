package instance

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const StateVersion = 1

var (
	ErrNotFound = errors.New("mcpx instance not found")
	ErrStale    = errors.New("mcpx instance is stale")
)

// State is the user-level rendezvous record for the one default MCPX Instance.
// Its location is intentionally independent of MCPX_HOME so shells with
// different MCPX_HOME values still discover the same running Runtime.
type State struct {
	Version    int    `json:"version"`
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
	Home       string `json:"home"`
	Addr       string `json:"addr"`
	Endpoint   string `json:"endpoint"`
	Build      string `json:"build,omitempty"`
	Commit     string `json:"commit,omitempty"`
	StartedAt  string `json:"started_at"`
}

func NewID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "mcpx_" + hex.EncodeToString(raw[:]), nil
}

// RuntimeDir is the default-instance rendezvous directory. MCPX_RUNTIME_DIR is
// an advanced override for packaging/tests; unlike MCPX_HOME it is not project
// or installation state.
func RuntimeDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("MCPX_RUNTIME_DIR")); value != "" {
		return filepath.Abs(value)
	}
	if value := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); value != "" {
		return filepath.Join(value, "mcpx"), nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "mcpx", "runtime"), nil
}

func StatePath() (string, error) {
	dir, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "default-instance.json"), nil
}

func Read() (State, error) {
	path, err := StatePath()
	if err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, ErrNotFound
		}
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode MCPX instance state %s: %w", path, err)
	}
	if err := state.Validate(); err != nil {
		return State{}, fmt.Errorf("invalid MCPX instance state %s: %w", path, err)
	}
	return state, nil
}

func (s State) Validate() error {
	if s.Version != StateVersion {
		return fmt.Errorf("unsupported instance state version %d", s.Version)
	}
	if strings.TrimSpace(s.InstanceID) == "" || s.PID <= 0 || strings.TrimSpace(s.Executable) == "" || strings.TrimSpace(s.Home) == "" || strings.TrimSpace(s.Endpoint) == "" {
		return errors.New("instance_id, pid, executable, home, and endpoint are required")
	}
	return nil
}

func Write(state State) error {
	if err := state.Validate(); err != nil {
		return err
	}
	path, err := StatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instance-*.tmp")
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
	return os.Rename(tmpPath, path)
}

// ResolveRunning returns the default Instance only when its recorded process
// still matches the executable that published the rendezvous state.
func ResolveRunning() (State, error) {
	state, err := Read()
	if err != nil {
		return State{}, err
	}
	running, err := processMatches(state.PID, state.Executable)
	if err != nil {
		return State{}, err
	}
	if !running {
		return State{}, fmt.Errorf("%w: pid %d", ErrStale, state.PID)
	}
	return state, nil
}

func RemoveIfOwned(instanceID string) error {
	state, err := Read()
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.InstanceID != strings.TrimSpace(instanceID) {
		return nil
	}
	path, err := StatePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func StartedAtNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
