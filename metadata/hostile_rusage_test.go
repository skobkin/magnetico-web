//go:build unix

package metadata

import (
	"os"
	"syscall"
)

func maxRSSKiB(ps *os.ProcessState) int64 {
	if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
		return ru.Maxrss // KiB on Linux
	}
	return 0
}
