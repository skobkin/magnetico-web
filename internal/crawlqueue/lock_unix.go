//go:build unix

package crawlqueue

import (
	"fmt"
	"os"
	"syscall"
)

// lockDir takes an exclusive, non-blocking flock on a lock file inside the
// queue directory. The lock lives as long as the returned file is open and is
// released by the kernel if the process dies.
func lockDir(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("crawlqueue: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("crawlqueue: lock %s: %w", path, err)
	}
	return f, nil
}

func unlockDir(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
