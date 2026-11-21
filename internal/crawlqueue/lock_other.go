//go:build !unix

package crawlqueue

import (
	"errors"
	"os"
)

func lockDir(string) (*os.File, error) {
	return nil, errors.New("crawlqueue: exclusive directory locking is only implemented on unix")
}

func unlockDir(*os.File) {}
