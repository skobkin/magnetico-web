//go:build diag

package app

import (
	"fmt"
	"strings"
)

// fmtSscanf reads the leading goroutine count of a profile block ("12 @ 0x…").
func fmtSscanf(block string, n *int) (int, error) {
	block = strings.TrimSpace(block)
	return fmt.Sscanf(block, "%d @", n)
}
