//go:build !unix

package metadata

import "os"

func maxRSSKiB(*os.ProcessState) int64 { return 0 }
