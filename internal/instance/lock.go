package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type lockOwner struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
}

type StartLock struct {
	dir string
}

// AcquireStartLock serializes discovery/start of the one default Instance.
// The lock lives beside the rendezvous state, so it is shared even when callers
// use different MCPX_HOME values.
func AcquireStartLock(timeout time.Duration) (*StartLock, error) {
	runtimeDir, err := RuntimeDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(runtimeDir, "default-instance.lock")
	deadline := time.Now().Add(timeout)
	for {
		if err := os.Mkdir(lockDir, 0o700); err == nil {
			executable, resolveErr := os.Executable()
			if resolveErr != nil {
				_ = os.Remove(lockDir)
				return nil, resolveErr
			}
			encoded, _ := json.Marshal(lockOwner{PID: os.Getpid(), Executable: executable})
			encoded = append(encoded, '\n')
			if writeErr := os.WriteFile(filepath.Join(lockDir, "owner.json"), encoded, 0o600); writeErr != nil {
				_ = os.Remove(lockDir)
				return nil, writeErr
			}
			return &StartLock{dir: lockDir}, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}

		stale, staleErr := staleStartLock(lockDir)
		if staleErr != nil {
			return nil, staleErr
		}
		if stale {
			_ = os.Remove(filepath.Join(lockDir, "owner.json"))
			if removeErr := os.Remove(lockDir); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return nil, removeErr
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for MCPX default Instance start lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func staleStartLock(lockDir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(lockDir, "owner.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Give a process that just created the directory a short grace period.
			info, statErr := os.Stat(lockDir)
			if statErr != nil {
				return errors.Is(statErr, os.ErrNotExist), nil
			}
			return time.Since(info.ModTime()) > time.Second, nil
		}
		return false, err
	}
	var owner lockOwner
	if err := json.Unmarshal(data, &owner); err != nil || owner.PID <= 0 || owner.Executable == "" {
		return true, nil
	}
	matches, err := processMatches(owner.PID, owner.Executable)
	if err != nil {
		return false, err
	}
	return !matches, nil
}

func (l *StartLock) Release() error {
	if l == nil || l.dir == "" {
		return nil
	}
	_ = os.Remove(filepath.Join(l.dir, "owner.json"))
	if err := os.Remove(l.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l.dir = ""
	return nil
}
