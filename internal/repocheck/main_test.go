package repocheck

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		_ = os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}
