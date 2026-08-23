package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrTimeout = errors.New("file lock timeout")

type Lock struct {
	file *os.File
}

func Acquire(path string, timeout time.Duration) (*Lock, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}

	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		locked, lockErr := tryLockExclusive(file)
		if lockErr != nil {
			_ = file.Close()
			return nil, lockErr
		}
		if locked {
			return &Lock{file: file}, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			_ = file.Close()
			return nil, ErrTimeout
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := unlockExclusive(file)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
