package instance

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"mcpx/internal/lockfile"
)

type StartLock struct {
	lock *lockfile.Lock
}

// AcquireStartLock serializes discovery/start of the one default Instance.
// The lock lives beside the rendezvous state, so it is shared even when callers
// use different MCPX_HOME values. OS-backed locking releases automatically when
// an owning process exits, so no stale-owner cleanup is required.
func AcquireStartLock(timeout time.Duration) (*StartLock, error) {
	runtimeDir, err := RuntimeDir()
	if err != nil {
		return nil, err
	}
	guard, err := lockfile.Acquire(filepath.Join(runtimeDir, "default-instance.start.lock"), timeout)
	if err != nil {
		if errors.Is(err, lockfile.ErrTimeout) {
			return nil, fmt.Errorf("timed out waiting for MCPX default Instance start lock")
		}
		return nil, err
	}
	return &StartLock{lock: guard}, nil
}

func (l *StartLock) Release() error {
	if l == nil || l.lock == nil {
		return nil
	}
	guard := l.lock
	l.lock = nil
	return guard.Release()
}
