package lockfile

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireSerializesAndTimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.lock")
	first, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	if _, err := Acquire(path, 75*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("second lock err=%v want timeout", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}
